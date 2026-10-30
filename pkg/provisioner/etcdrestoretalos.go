package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// talosRestorePoll is how often a Talos restore looks at the nodes it waits for.
	talosRestorePoll = 5 * time.Second
	// talosEtcdRunning and talosEtcdPreparing are the states Talos gives its etcd service: running,
	// and waiting for a cluster to join or to be bootstrapped, as it does after a wipe.
	talosEtcdRunning   = "Running"
	talosEtcdPreparing = "Preparing"
)

// planTalosRestore writes out the steps that restore a Talos cluster's etcd by hand with talosctl,
// as the controller does through the Talos API.
func planTalosRestore(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster, snapshot *EtcdSnapshot) (*EtcdRestorePlan, error) {
	nodes, err := controlPlaneMachines(ctx, c, cluster)
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if a := machineAddress(n.object); a != "" {
			addresses = append(addresses, a)
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("no control-plane machine of the cluster has an address yet")
	}
	const (
		mgmt     = "the management cluster"
		here     = "the machine the snapshot was downloaded to, which reaches the nodes' Talos API"
		talosctl = "talosctl --talosconfig talosconfig --nodes "
	)
	patch := fmt.Sprintf("kubectl --namespace %s patch cluster %s --type merge --patch ", cluster.Namespace, cluster.Name)
	return &EtcdRestorePlan{Snapshot: *snapshot, Distribution: string(templates.Talos), Steps: []RestoreStep{
		{"Pause the cluster, so that Cluster API replaces no machine meanwhile", mgmt, patch + `'{"spec":{"paused":true}}'`},
		{
			"Write the cluster's talosconfig beside the snapshot", mgmt,
			fmt.Sprintf("kubectl --namespace %s get secret %s --output jsonpath='{.data.%s}' | base64 --decode > talosconfig",
				cluster.Namespace, TalosconfigSecret(cluster.Name), TalosconfigKey),
		},
		{"Save the downloaded snapshot as db.snapshot, and check it", here, fmt.Sprintf("echo '%s  db.snapshot' | sha256sum -c -", snapshot.SHA256)},
		{
			"Wipe etcd on every control-plane node, which reboots each", here,
			talosctl + strings.Join(addresses, ",") + " reset --graceful=false --reboot --system-labels-to-wipe=EPHEMERAL",
		},
		{"Wait until etcd is Preparing, waiting to be bootstrapped", here, talosctl + addresses[0] + " service etcd"},
		{"Restore etcd from the snapshot; the other nodes join it again by themselves", here, talosctl + addresses[0] + " bootstrap --recover-from=db.snapshot"},
		{"Let Cluster API look after the cluster again", mgmt, patch + `'{"spec":{"paused":false}}'`},
	}}, nil
}

// prepareTalos checks the snapshot and reaches every control-plane node's Talos API.
func (run *restoreRun) prepareTalos(ctx context.Context) error {
	if err := run.knowTalos(ctx); err != nil {
		return err
	}
	if err := run.readSnapshot(ctx); err != nil {
		return err
	}
	nodes, err := controlPlaneMachines(ctx, run.r.Client, run.cluster)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		n.Address = machineAddress(n.object)
		if n.Address == "" {
			return fmt.Errorf("control-plane machine %s has no address yet. Restore once all are running", n.Machine)
		}
		// A node whose API does not answer is left out and made again: its etcd, should it run, is
		// of the cluster before the restore, whose members the restored one does not let in.
		answers := func(ctx context.Context, t TalosNode) error {
			_, err := t.BootID(ctx)
			return err //nolint:wrapcheck // onTalos names the machine, and the node's error the call
		}
		if err := run.onTalos(ctx, n, answers); err != nil {
			ctrl.LoggerFrom(ctx).Info("left out of the restore", "machine", n.Machine, "error", err)
			run.status.Replaced = append(run.status.Replaced, n.Machine)
			continue
		}
		run.nodes = append(run.nodes, n)
	}
	if len(run.nodes) == 0 {
		return errors.New("no control-plane node of the cluster answers on its Talos API")
	}
	run.status.Machine, run.status.Node = run.nodes[0].Machine, run.nodes[0].Node
	return nil
}

// knowTalos reads the cluster's talosconfig.
func (run *restoreRun) knowTalos(ctx context.Context) error {
	if run.r.Talos == nil {
		return errors.New("this controller does not reach Talos nodes")
	}
	raw, err := readTalosconfig(ctx, run.r.Client, run.key.Namespace, run.key.Name)
	if err != nil {
		return err
	}
	run.talosconfig = raw
	return nil
}

// checkSnapshot reads the whole snapshot before anything is wiped, and checks it against its
// checksum.
func (run *restoreRun) checkSnapshot(ctx context.Context) error {
	return run.withSnapshot(ctx, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err //nolint:wrapcheck // withSnapshot says which snapshot
	})
}

// withSnapshot hands the snapshot to use, and fails when what it read does not match the
// snapshot's checksum.
func (run *restoreRun) withSnapshot(ctx context.Context, use func(io.Reader) error) error {
	plain, err := run.target.OpenEtcdSnapshot(ctx, run.snapshot.Name)
	if err != nil {
		return err
	}
	defer func() { _ = plain.Close() }()
	sum := sha256.New()
	if err := use(io.TeeReader(plain, sum)); err != nil {
		return fmt.Errorf("snapshot %s: %w", run.snapshot.Name, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != run.snapshot.SHA256 {
		return fmt.Errorf("snapshot %s does not match its checksum", run.snapshot.Name)
	}
	return nil
}

// wipeTalos resets every control-plane node with etcd's data wiped, and waits until each has booted
// again. No etcd member of the cluster is then left, which the restored one could take back.
func (run *restoreRun) wipeTalos(ctx context.Context) error {
	boots := make([]string, len(run.nodes))
	for i, n := range run.nodes {
		err := run.waitTalos(ctx, n, "wiping etcd", func(ctx context.Context, t TalosNode) (bool, error) {
			boot, err := t.BootID(ctx)
			if err != nil {
				return false, err //nolint:wrapcheck // onTalos names the machine, and the node's error the call
			}
			if err := t.WipeEtcd(ctx); err != nil {
				return false, err //nolint:wrapcheck // as above
			}
			boots[i] = boot
			return true, nil
		})
		if err != nil {
			return err
		}
	}
	for i, n := range run.nodes {
		err := run.waitTalos(ctx, n, "booting with etcd wiped", func(ctx context.Context, t TalosNode) (bool, error) {
			boot, err := t.BootID(ctx)
			return boot != boots[i], err //nolint:wrapcheck // onTalos names the machine, and the node's error the call
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// recoverTalosEtcd uploads the snapshot to the first node once its etcd waits for one, and
// bootstraps etcd from it.
func (run *restoreRun) recoverTalosEtcd(ctx context.Context) error {
	first := run.nodes[0]
	err := run.waitTalos(ctx, first, "uploading the snapshot", func(ctx context.Context, t TalosNode) (bool, error) {
		state, _, err := t.Etcd(ctx)
		if err != nil || state != talosEtcdPreparing {
			return false, err //nolint:wrapcheck // onTalos names the machine, and the node's error the call
		}
		return true, run.withSnapshot(ctx, func(r io.Reader) error { return t.UploadEtcdSnapshot(ctx, r) })
	})
	if err != nil {
		return err
	}
	return run.waitTalos(ctx, first, "restoring etcd from the snapshot", func(ctx context.Context, t TalosNode) (bool, error) {
		return true, t.BootstrapRecovered(ctx)
	})
}

// waitTalosEtcd waits until etcd runs on the first node, restored, and is healthy.
func (run *restoreRun) waitTalosEtcd(ctx context.Context) error {
	return run.waitTalos(ctx, run.nodes[0], "starting etcd", func(ctx context.Context, t TalosNode) (bool, error) {
		state, healthy, err := t.Etcd(ctx)
		return state == talosEtcdRunning && healthy, err //nolint:wrapcheck // onTalos names the machine, and the node's error the call
	})
}

// recoverTalos finishes what a Talos restore that failed did, by the phase it got to. Before
// anything was wiped, it only unpauses the cluster. Once a node may have been wiped, only the
// snapshot brings etcd back, so the restore is tried once more from the start.
func (run *restoreRun) recoverTalos(ctx context.Context, cause error) error {
	errs := []error{cause}
	if run.status.Phase == RestorePreparing || run.status.Phase == RestoreFetching {
		return errors.Join(append(errs, run.unpause(ctx))...)
	}
	if err := run.knowTalosNodes(ctx); err != nil {
		errs = append(errs, err, errors.New("control-plane nodes left unchanged. Restore by hand as the steps say"))
		return errors.Join(append(errs, run.unpause(ctx))...)
	}
	switch run.status.Phase {
	case RestoreStopping, RestoreRestoring:
		ctrl.LoggerFrom(ctx).Info("restoring etcd once more", "cause", cause)
		again, cancel := context.WithTimeout(ctx, etcdRestoreTimeout)
		defer cancel()
		var err error
		for _, step := range []func(context.Context) error{run.check, run.stop, run.restore, run.start} {
			if err = step(again); err != nil {
				break
			}
		}
		if err != nil {
			errs = append(errs, err, errors.New("etcd may be wiped. Restore again, or by hand as the steps say"))
			return errors.Join(append(errs, run.unpause(ctx))...)
		}
		run.retried = cause
		return errors.Join(run.replace(ctx), run.unpause(ctx))
	case RestoreStarting:
		errs = append(errs, run.waitTalosEtcd(ctx))
	case RestorePreparing, RestoreFetching, RestoreReplacing, RestoreSucceeded, RestoreFailed:
	}
	return errors.Join(append(errs, run.replace(ctx), run.unpause(ctx))...)
}

// check reads the snapshot again for a restore tried once more, the target with it.
func (run *restoreRun) check(ctx context.Context) error {
	if run.target == nil {
		if err := run.readSnapshot(ctx); err != nil {
			return err
		}
	}
	return run.checkSnapshot(ctx)
}

// knowTalosNodes reads the nodes again for a restore the controller restarted in: the one etcd is
// restored on first, and every other one not left out.
func (run *restoreRun) knowTalosNodes(ctx context.Context) error {
	if run.nodes != nil {
		return nil
	}
	if err := run.knowTalos(ctx); err != nil {
		return err
	}
	nodes, err := controlPlaneMachines(ctx, run.r.Client, run.cluster)
	if err != nil {
		return err
	}
	left := map[string]bool{}
	for _, m := range run.status.Replaced {
		left[m] = true
	}
	for _, n := range nodes {
		n.Address = machineAddress(n.object)
		switch {
		case n.Address == "" || left[n.Machine]:
		case n.Machine == run.status.Machine:
			run.nodes = append([]RestoreNode{n}, run.nodes...)
		default:
			run.nodes = append(run.nodes, n)
		}
	}
	if len(run.nodes) == 0 || run.nodes[0].Machine != run.status.Machine {
		return fmt.Errorf("control-plane machine %s, which etcd was restored on, is gone", run.status.Machine)
	}
	return nil
}

// onTalos does something through a node's Talos API.
func (run *restoreRun) onTalos(ctx context.Context, n RestoreNode, do func(context.Context, TalosNode) error) error {
	t, err := run.r.Talos(ctx, run.talosconfig, n.Address)
	if err != nil {
		return fmt.Errorf("control-plane machine %s: %w", n.Machine, err)
	}
	defer func() { _ = t.Close() }()
	if err := do(ctx, t); err != nil {
		return fmt.Errorf("control-plane machine %s: %w", n.Machine, err)
	}
	return nil
}

// waitTalos does something through a node's Talos API until it says it is done, for at most
// guestStepTimeout. A node that does not answer, or is not ready for it yet, is asked again;
// any other failure ends the wait.
func (run *restoreRun) waitTalos(ctx context.Context, n RestoreNode, what string, do func(context.Context, TalosNode) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, guestStepTimeout)
	defer cancel()
	poll := run.r.poll
	if poll == 0 {
		poll = talosRestorePoll
	}
	for {
		done := false
		err := run.onTalos(ctx, n, func(ctx context.Context, t TalosNode) error {
			var err error
			done, err = do(ctx, t)
			return err
		})
		switch {
		case err == nil && done:
			ctrl.LoggerFrom(ctx).Info(what, "machine", n.Machine)
			return nil
		case err != nil && !errors.Is(err, errTalosNotReady) && ctx.Err() == nil:
			return fmt.Errorf("%s: %w", what, err)
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
			return fmt.Errorf("%s on %s did not finish: %w", what, n.Machine, err)
		case <-time.After(poll):
		}
	}
}
