package provisioner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
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
// and waits until it runs. The others then join. open hands the plain snapshot, checked already.
func (m *ManagementTalos) Restore(ctx context.Context, admin []byte, nodes []string, open func() (io.ReadCloser, error), log func(string)) error {
	up := m.answering(ctx, admin, nodes, log)
	if len(up) == 0 {
		return errors.New("no control-plane node answers on its Talos API")
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
	log("Uploading the snapshot to " + first)
	err := m.wait(ctx, admin, first, "uploading the snapshot", func(ctx context.Context, t TalosNode) (bool, error) {
		state, _, err := t.Etcd(ctx)
		if err != nil || state != talosEtcdPreparing {
			return false, err //nolint:wrapcheck // wait names the node and what it was doing
		}
		r, err := open()
		if err != nil {
			return false, err
		}
		defer func() { _ = r.Close() }()
		return true, t.UploadEtcdSnapshot(ctx, r)
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
	return m.wait(ctx, admin, first, "starting etcd", func(ctx context.Context, t TalosNode) (bool, error) {
		state, healthy, err := t.Etcd(ctx)
		return state == talosEtcdRunning && healthy, err //nolint:wrapcheck // wait names the node and what it was doing
	})
}

func (*ManagementTalos) client(ctx context.Context, raw []byte, address string) (*talosclient.Client, error) {
	cfg, err := talosconfig.FromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("reading the talosconfig: %w", err)
	}
	c, err := talosclient.New(ctx, talosclient.WithConfig(cfg),
		talosclient.WithEndpoints(net.JoinHostPort(address, strconv.Itoa(talosAPIPort))))
	if err != nil {
		return nil, fmt.Errorf("reaching the Talos API of %s: %w", address, err)
	}
	return c, nil
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
