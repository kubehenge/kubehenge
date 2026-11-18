package provisioner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/role"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ManagementEtcdFolder is the folder of the management cluster's etcd snapshots in a target, beside
// the clusters' folders, whose names hold a dot this one does not.
func ManagementEtcdFolder(prefix string) string {
	return path.Join(prefix, "management", EtcdSnapshotFolder)
}

// ManagementEtcdIdentity is the object beside the management cluster's snapshots that holds the age
// identity they are encrypted to, itself encrypted with the appliance's passphrase, so that the
// snapshots can be restored without the appliance that took them.
func ManagementEtcdIdentity(prefix string) string {
	return path.Join(prefix, "management", "identity.age")
}

// TalosEndpoints are the control-plane addresses a talosconfig's current context names.
func TalosEndpoints(raw []byte) ([]string, error) {
	cfg, err := talosconfig.FromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("reading the talosconfig: %w", err)
	}
	ctx := cfg.Contexts[cfg.Context]
	if ctx == nil || len(ctx.Endpoints) == 0 {
		return nil, errors.New("the talosconfig names no endpoint")
	}
	return ctx.Endpoints, nil
}

// ManagementTalos reaches the management cluster's control-plane nodes for its etcd backups.
type ManagementTalos struct {
	// Dial reaches a node for a restore. It is DialTalos when nil.
	Dial TalosDialer
	// Poll and StepTimeout pace a restore's waits. They are talosRestorePoll and guestStepTimeout when 0.
	Poll, StepTimeout time.Duration
}

// BackupConfig has a node make a talosconfig that may take etcd snapshots and nothing else, valid
// for ttl, with an admin talosconfig.
func (m *ManagementTalos) BackupConfig(ctx context.Context, admin []byte, address string, ttl time.Duration) ([]byte, error) {
	c, err := m.client(ctx, admin, address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	resp, err := c.GenerateClientConfiguration(ctx, &machine.GenerateClientConfigurationRequest{
		Roles:  []string{string(role.EtcdBackup)},
		CrtTtl: durationpb.New(ttl),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: making a talosconfig for etcd snapshots: %w", address, err)
	}
	for _, msg := range resp.GetMessages() {
		if raw := msg.GetTalosconfig(); len(raw) > 0 {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("%s: made no talosconfig for etcd snapshots", address)
}

// Snapshot streams a snapshot of etcd from the first node that answers, with a talosconfig that
// may hold no more than the role to take one.
func (m *ManagementTalos) Snapshot(ctx context.Context, raw []byte, nodes []string) (io.ReadCloser, error) {
	var errs []error
	for _, address := range nodes {
		c, err := m.client(ctx, raw, address)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r, err := c.EtcdSnapshot(ctx, &machine.EtcdSnapshotRequest{})
		if err != nil {
			_ = c.Close()
			errs = append(errs, fmt.Errorf("%s: etcd snapshot: %w", address, err))
			continue
		}
		return closeBoth{ReadCloser: r, conn: c}, nil
	}
	return nil, errors.Join(errs...)
}

// Restore restores the management cluster's etcd from a snapshot through the Talos API, as an
// etcd restore of a Talos cluster does: it wipes etcd on every control-plane node that answers,
// waits for each to boot again, uploads the snapshot to the first and bootstraps etcd from it,
// and waits until it runs and the others have joined it. open hands the plain snapshot, checked
// already.
func (m *ManagementTalos) Restore(ctx context.Context, admin []byte, nodes []string, open func() (io.ReadCloser, error), log func(string)) error {
	up := m.answering(ctx, admin, nodes, log)
	if len(up) == 0 {
		return errors.New("no control-plane node answers on its Talos API: start the management cluster's VMs in Proxmox VE, or if they are gone, recreate the management cluster from this snapshot")
	}
	boots := make([]string, len(up))
	for i, address := range up {
		log("Wiping etcd on " + address)
		err := m.wait(ctx, admin, address, "wiping etcd", func(ctx context.Context, t TalosNode) (bool, error) {
			boot, err := t.BootID(ctx)
			if err != nil {
				return false, err //nolint:wrapcheck // wait names the node and what it was doing
			}
			boots[i] = boot
			return true, t.WipeEtcd(ctx)
		})
		if err != nil {
			return err
		}
	}
	for i, address := range up {
		err := m.wait(ctx, admin, address, "booting with etcd wiped", func(ctx context.Context, t TalosNode) (bool, error) {
			boot, err := t.BootID(ctx)
			return boot != boots[i], err //nolint:wrapcheck // wait names the node and what it was doing
		})
		if err != nil {
			return err
		}
	}
	first := up[0]
	upload := func(ctx context.Context, t TalosNode) error {
		r, err := open()
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		return t.UploadEtcdSnapshot(ctx, r)
	}
	log("Uploading the snapshot to " + first)
	err := m.wait(ctx, admin, first, "uploading the snapshot", func(ctx context.Context, t TalosNode) (bool, error) {
		etcd, err := t.Etcd(ctx)
		if err != nil || !talosEtcdTakesSnapshot(etcd.State) {
			return false, err //nolint:wrapcheck // wait names the node and what it was doing
		}
		return true, upload(ctx, t)
	})
	if err != nil {
		return err
	}
	log("Restoring etcd from the snapshot on " + first)
	err = m.wait(ctx, admin, first, "restoring etcd from the snapshot", func(ctx context.Context, t TalosNode) (bool, error) {
		return true, t.BootstrapRecovered(ctx)
	})
	if err != nil {
		return err
	}
	log("Waiting for etcd on " + first)
	err = m.wait(ctx, admin, first, "starting etcd", func(ctx context.Context, t TalosNode) (bool, error) {
		return talosEtcdStarted(ctx, t, func(ctx context.Context, t TalosNode) error {
			log("Starting etcd again on " + first + " after Talos left it Failed")
			return upload(ctx, t)
		})
	})
	if err != nil {
		return err
	}
	return m.waitJoined(ctx, admin, up[1:], log)
}

// EtcdStart is what StartFirstEtcd keeps of one node's etcd while a new management cluster starts.
type EtcdStart struct {
	// Boot is the node's boot of the last reboot, which is made once per boot.
	Boot    string `json:"boot,omitempty"`
	Repairs int    `json:"repairs,omitempty"`
	GaveUp  bool   `json:"gaveUp,omitempty"`
	// Ran is set once etcd was seen running on the node, which then holds data.
	Ran bool `json:"ran,omitempty"`
}

// StartFirstEtcd looks once at etcd on each node of a management cluster whose first apply runs,
// nodes[0] the one the module bootstraps, and starts etcd again where Talos left it Failed, as the
// workload clusters' etcd watch does. nodes[0] is bootstrapped again while etcd has run on no node.
// Any other node, or one whose bootstrap Talos refuses, is rebooted. A node is rebooted once per
// boot, started at most maxEtcdRepairs times, and never once its etcd ran. starts holds the count by node,
// and record writes it down before each start, so that an appliance that stops between the two
// counts a start it never made rather than makes one twice. A node that does not answer is looked
// at next time.
func (m *ManagementTalos) StartFirstEtcd(ctx context.Context, admin []byte, nodes []string, starts map[string]EtcdStart, record func() error, log func(string)) {
	ran := false
	for _, address := range nodes {
		m.onNode(ctx, admin, address, func(ctx context.Context, t TalosNode) {
			if etcd, err := t.Etcd(ctx); err == nil && etcd.State == talosEtcdRunning && !starts[address].Ran {
				s := starts[address]
				s.Ran = true
				starts[address] = s
				_ = record() // the next record writes it too
			}
		})
		ran = ran || starts[address].Ran
	}
	for i, address := range nodes {
		start := rebootEtcd
		if i == 0 && !ran {
			start = bootstrapEtcd
		}
		m.onNode(ctx, admin, address, func(ctx context.Context, t TalosNode) {
			if err := startFirstEtcd(ctx, t, address, start, starts, record, log); err != nil {
				log(fmt.Sprintf("Starting etcd again on %s failed: %v", address, err))
			}
		})
	}
}

// bootstrapEtcd bootstraps etcd on the node and says so, or reboots the node where Talos refuses.
// A node not ready for a bootstrap is left as it is. A bootstrap leaves the boot unrecorded, so that
// an etcd failing once more in the same boot is bootstrapped again, as the workload watch does.
func bootstrapEtcd(ctx context.Context, t TalosNode, rebooting func() error) (string, error) {
	err := t.Bootstrap(ctx)
	switch {
	case err == nil:
		return ". Bootstrapped it again.", nil
	case errors.Is(err, errTalosNotReady):
		return "", err //nolint:wrapcheck // the node's error names the node and the call
	}
	done, rebootErr := rebootEtcd(ctx, t, rebooting)
	return fmt.Sprintf(". Talos refused to bootstrap it again: %v%s", err, done), rebootErr
}

// rebootEtcd records the boot with rebooting, then reboots the node and says so.
func rebootEtcd(ctx context.Context, t TalosNode, rebooting func() error) (string, error) {
	if err := rebooting(); err != nil {
		return "", err
	}
	return ". Rebooted the node to start etcd again.", t.Reboot(ctx) //nolint:wrapcheck // the node's error names the node and the call
}

// onNode does something on a node within watchReadTimeout, if its Talos API answers.
func (m *ManagementTalos) onNode(ctx context.Context, admin []byte, address string, do func(context.Context, TalosNode)) {
	ctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	_ = m.on(ctx, admin, address, func(ctx context.Context, t TalosNode) error {
		do(ctx, t)
		return nil
	})
}

// startFirstEtcd starts etcd on one node again with start if Talos left it Failed.
func startFirstEtcd(ctx context.Context, t TalosNode, address string, start func(context.Context, TalosNode, func() error) (string, error), starts map[string]EtcdStart, record func() error, log func(string)) error {
	s := starts[address]
	if s.Ran || s.GaveUp {
		return nil
	}
	etcd, err := t.Etcd(ctx)
	if err != nil || etcd.State != talosEtcdFailed {
		return nil //nolint:nilerr // a node not ready yet is looked at next time
	}
	boot, err := t.BootID(ctx)
	if err != nil || boot == s.Boot {
		return nil //nolint:nilerr // as above
	}
	note := "Talos left etcd on " + address + " Failed"
	if etcd.Event != "" {
		note += ": " + etcd.Event
	}
	if s.Repairs >= maxEtcdRepairs {
		gaveUp := s
		gaveUp.GaveUp = true
		starts[address] = gaveUp
		if err = record(); err != nil {
			return err
		}
		log(fmt.Sprintf("%s. Left it Failed after %d starts.", note, s.Repairs))
		return nil
	}
	counted := EtcdStart{Boot: s.Boot, Repairs: s.Repairs + 1}
	starts[address] = counted
	if err = record(); err != nil {
		return err
	}
	done, err := start(ctx, t, func() error {
		counted.Boot = boot
		starts[address] = counted
		return record()
	})
	switch {
	case errors.Is(err, errTalosNotReady):
		// Not a start, so it does not count.
		starts[address] = s
		return record()
	case err != nil:
		return err
	}
	log(note + done)
	return nil
}

// waitJoined waits until etcd on each of the nodes has joined the restored one.
func (m *ManagementTalos) waitJoined(ctx context.Context, admin []byte, nodes []string, log func(string)) error {
	for _, address := range nodes {
		log("Waiting for " + address + " to join etcd")
		from := ""
		err := m.wait(ctx, admin, address, "joining etcd", func(ctx context.Context, t TalosNode) (bool, error) {
			return talosEtcdJoined(ctx, t, &from, func(ctx context.Context, t TalosNode) error {
				log("Rebooting " + address + " after Talos left its etcd Failed")
				return t.Reboot(ctx)
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (*ManagementTalos) client(ctx context.Context, raw []byte, address string) (*talosclient.Client, error) {
	n, err := connectTalos(ctx, raw, address)
	if err != nil {
		return nil, err
	}
	return n.c, nil
}

// answering is those of the nodes whose Talos API answers. The others are logged as left out.
func (m *ManagementTalos) answering(ctx context.Context, admin []byte, nodes []string, log func(string)) []string {
	var up []string
	for _, address := range nodes {
		err := m.on(ctx, admin, address, func(ctx context.Context, t TalosNode) error {
			_, err := t.BootID(ctx)
			return err //nolint:wrapcheck // on names the node, and the node's error the call
		})
		if err != nil {
			log(fmt.Sprintf("Leaving %s out: %v", address, err))
			continue
		}
		up = append(up, address)
	}
	return up
}

func (m *ManagementTalos) on(ctx context.Context, raw []byte, address string, do func(context.Context, TalosNode) error) error {
	dial := m.Dial
	if dial == nil {
		dial = DialTalos
	}
	t, err := dial(ctx, raw, address)
	if err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	defer func() { _ = t.Close() }()
	if err := do(ctx, t); err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	return nil
}

// wait does something on a node until it says it is done. A node that does not answer, or is not
// ready for it yet, is asked again. Any other failure ends the wait.
func (m *ManagementTalos) wait(ctx context.Context, raw []byte, address, what string, do func(context.Context, TalosNode) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(m.StepTimeout, guestStepTimeout))
	defer cancel()
	poll := cmp.Or(m.Poll, talosRestorePoll)
	for {
		done := false
		err := m.on(ctx, raw, address, func(ctx context.Context, t TalosNode) error {
			var err error
			done, err = do(ctx, t)
			return err
		})
		switch {
		case err == nil && done:
			return nil
		case err != nil && !errors.Is(err, errTalosNotReady) && ctx.Err() == nil:
			return fmt.Errorf("%s: %w", what, err)
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
			return fmt.Errorf("%s on %s did not finish: %w", what, address, err)
		case <-time.After(poll):
		}
	}
}
