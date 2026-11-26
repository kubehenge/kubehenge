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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		etcd, err := t.Service(ctx, talosServiceEtcd)
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
	Boot string `json:"boot,omitempty"`
	// Bootstrapped is the boot the first node was bootstrapped in after a reboot, which is done once
	// per boot. It is kept apart from Boot, so that a node failing again after that bootstrap is
	// still rebooted in its boot.
	Bootstrapped string `json:"bootstrapped,omitempty"`
	Repairs      int    `json:"repairs,omitempty"`
	GaveUp       bool   `json:"gaveUp,omitempty"`
	// Ran is set once etcd was seen running on the node, which then holds data.
	Ran bool `json:"ran,omitempty"`
}

// StartFirstEtcd looks once at etcd and the kubelet on each node of a management cluster whose first
// apply runs, nodes[0] the one the module bootstraps, and starts etcd again where Talos left it
// Failed, as the workload clusters' etcd watch does. Talos gives up an image pull after 20 minutes,
// as when a site CA was missing, and starts neither service again, so a node whose kubelet is Failed
// is rebooted too, which also has its container runtime read the trust roots anew. nodes[0] is
// bootstrapped again while etcd has run on no node, and once after a reboot into a newer boot.
// Any other node, one whose kubelet is Failed, or one whose bootstrap Talos refuses, is rebooted. A
// node is rebooted once per boot and started at most maxEtcdRepairs times. Once its etcd ran it is
// rebooted only for a Failed kubelet, since a reboot keeps etcd's data and without the kubelet
// kube-apiserver never runs. starts holds the count by node,
// and record writes it down before each start, so that an appliance that stops between the two
// counts a start it never made rather than makes one twice. A reboot's boot is written down only
// once the reboot was made, so that a reboot cut off is made on a later look. A node that does not
// answer is looked at next time.
func (m *ManagementTalos) StartFirstEtcd(ctx context.Context, admin []byte, nodes []string, starts map[string]EtcdStart, record func() error, log func(string)) {
	ran := false
	for _, address := range nodes {
		m.onNode(ctx, admin, address, func(ctx context.Context, t TalosNode) {
			if etcd, err := t.Service(ctx, talosServiceEtcd); err == nil && etcd.State == talosEtcdRunning && !starts[address].Ran {
				s := starts[address]
				s.Ran = true
				starts[address] = s
				_ = record() // the next record writes it too
			}
		})
		ran = ran || starts[address].Ran
	}
	for i, address := range nodes {
		seat := joiner
		if i == 0 && !ran {
			seat = seed
		}
		m.onNode(ctx, admin, address, func(call context.Context, t TalosNode) {
			// A stopped run says nothing of the start it cut off.
			if err := startFirstEtcd(call, t, address, seat, starts, record, log); err != nil && ctx.Err() == nil {
				log(fmt.Sprintf("Starting etcd again on %s failed: %v", address, err))
			}
		})
	}
}

// bootstrapEtcd bootstraps etcd on the node and says so, or reboots the node where Talos refuses.
// A node not ready for a bootstrap, or a call cut off before Talos answered, is left as it is. A
// bootstrap leaves the boot unrecorded, so that an etcd failing once more in the same boot is
// bootstrapped again, as the workload watch does.
func bootstrapEtcd(ctx context.Context, t TalosNode, rebooted func() error) (string, error) {
	err := t.Bootstrap(ctx)
	switch {
	case err == nil:
		return ". Bootstrapped it again.", nil
	case errors.Is(err, errTalosNotReady), unanswered(ctx, err):
		return "", err //nolint:wrapcheck // the node's error names the node and the call
	}
	done, rebootErr := rebootEtcd(ctx, t, rebooted)
	if rebootErr != nil {
		return "", rebootErr
	}
	return fmt.Sprintf(". Talos refused to bootstrap it again: %v%s", err, done), nil
}

// rebootEtcd reboots the node, then records its boot with rebooted, and says so.
func rebootEtcd(ctx context.Context, t TalosNode, rebooted func() error) (string, error) {
	if err := t.Reboot(ctx); err != nil {
		return "", err //nolint:wrapcheck // the node's error names the node and the call
	}
	return ". Rebooted the node to start etcd again.", rebooted()
}

// rebootKubelet reboots a node whose etcd ran for its kubelet, as rebootEtcd does.
func rebootKubelet(ctx context.Context, t TalosNode, rebooted func() error) (string, error) {
	done, err := rebootEtcd(ctx, t, rebooted)
	if done == "" {
		return "", err
	}
	return ". Rebooted the node to start the kubelet again.", err
}

// unanswered reports whether a call ended before Talos answered it: the run stopped, or the call
// ran out of time. Neither is Talos refusing it.
func unanswered(ctx context.Context, err error) bool {
	switch status.Code(err) { //nolint:exhaustive // every other code is Talos's answer
	case codes.Canceled, codes.DeadlineExceeded:
		return true
	}
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
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

// nodeRole says whether a node is the one the module bootstraps.
type nodeRole int

const (
	joiner nodeRole = iota
	seed
)

// nodeLook is what one look at a node found, all of one boot.
type nodeLook struct {
	boot          string
	etcd, kubelet TalosService
}

// readNode reads the node's boot, then its services, then the boot again. A node that rebooted
// between the reads says nothing of the boot it is in, so its services are read on a later look.
func readNode(ctx context.Context, t TalosNode) (nodeLook, error) {
	boot, err := t.BootID(ctx)
	if err != nil {
		return nodeLook{}, err //nolint:wrapcheck // the node's error names the node and the call
	}
	etcd, err := t.Service(ctx, talosServiceEtcd)
	if err != nil {
		return nodeLook{}, err //nolint:wrapcheck // as above
	}
	kubelet, err := t.Service(ctx, talosServiceKubelet)
	if err != nil {
		return nodeLook{}, err //nolint:wrapcheck // as above
	}
	again, err := t.BootID(ctx)
	if err != nil {
		return nodeLook{}, err //nolint:wrapcheck // as above
	}
	if again != boot {
		return nodeLook{}, errTalosNotReady
	}
	return nodeLook{boot: boot, etcd: etcd, kubelet: kubelet}, nil
}

// startFirstEtcd starts etcd on one node again if Talos left it or the kubelet Failed. The first node
// is bootstrapped where only etcd failed and it is the seed, and any other is rebooted.
func startFirstEtcd(ctx context.Context, t TalosNode, address string, seat nodeRole, starts map[string]EtcdStart, record func() error, log func(string)) error {
	s := starts[address]
	if s.GaveUp {
		return nil
	}
	seen, err := readNode(ctx, t)
	if err != nil {
		return nil //nolint:nilerr // a node not ready yet is looked at next time
	}
	boot, etcd, kubelet := seen.boot, seen.etcd, seen.kubelet
	ran := s.Ran || etcd.State == talosEtcdRunning
	etcdFailed, kubeletFailed := !ran && etcd.State == talosEtcdFailed, kubelet.State == talosEtcdFailed
	if !etcdFailed && !kubeletFailed {
		// Only a node rebooted into a newer boot waits for a bootstrap, so that none is made on an
		// etcd that other starts are about to give one.
		if seat == seed && !ran && etcd.State == talosEtcdPreparing && s.Boot != "" && boot != s.Boot && boot != s.Bootstrapped {
			return bootstrapAfterReboot(ctx, t, address, boot, starts, record, log)
		}
		return nil
	}
	if boot == s.Boot {
		return nil
	}
	start := rebootEtcd
	switch {
	case ran:
		start = rebootKubelet
	case seat == seed && !kubeletFailed:
		start = bootstrapEtcd
	}
	note := "Talos left " + failedServices(etcdFailed, kubeletFailed) + " on " + address + " Failed"
	event := etcd.Event
	if !etcdFailed {
		event = kubelet.Event
	}
	if event != "" {
		note += ": " + event
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
	counted := s
	counted.Repairs++
	starts[address] = counted
	if err = record(); err != nil {
		return err
	}
	done, err := start(ctx, t, func() error {
		counted.Boot = boot
		starts[address] = counted
		return record()
	})
	if errors.Is(err, errTalosNotReady) {
		// Not a start, so it does not count.
		starts[address] = s
		return record()
	}
	if done != "" {
		log(note + done)
	}
	return err
}

// failedServices names the services Talos left Failed.
func failedServices(etcd, kubelet bool) string {
	switch {
	case etcd && kubelet:
		return "etcd and the kubelet"
	case kubelet:
		return "the kubelet"
	}
	return "etcd"
}

// bootstrapAfterReboot bootstraps etcd on the first node, which a reboot of the watch's brought up
// waiting, once in its boot. The boot is written down before the call, so that no look bootstraps
// again in it. A bootstrap is no repair, since only a reboot, which is one, leads to it. A node not
// ready for it is bootstrapped on a later look, as bootstrapEtcd leaves one, and says nothing.
func bootstrapAfterReboot(ctx context.Context, t TalosNode, address, boot string, starts map[string]EtcdStart, record func() error, log func(string)) error {
	s := starts[address]
	marked := s
	marked.Bootstrapped = boot
	starts[address] = marked
	if err := record(); err != nil {
		return err
	}
	err := t.Bootstrap(ctx)
	switch {
	case err == nil:
		log("Bootstrapped etcd on " + address + " after its reboot.")
		return nil
	case errors.Is(err, errTalosNotReady), unanswered(ctx, err):
		starts[address] = s
		if recordErr := record(); recordErr != nil || errors.Is(err, errTalosNotReady) {
			return recordErr
		}
		return err //nolint:wrapcheck // the node's error names the node and the call
	}
	log(fmt.Sprintf("Talos refused to bootstrap etcd on %s after its reboot: %v", address, err))
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
	if err := m.with(ctx, raw, address, do); err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	return nil
}

// with is on with an error that does not name the node, for a caller that does.
func (m *ManagementTalos) with(ctx context.Context, raw []byte, address string, do func(context.Context, TalosNode) error) error {
	dial := m.Dial
	if dial == nil {
		dial = DialTalos
	}
	t, err := dial(ctx, raw, address)
	if err != nil {
		return err
	}
	defer func() { _ = t.Close() }()
	return do(ctx, t)
}

// wait does something on a node until it says it is done. A node that does not answer, or is not
// ready for it yet, is asked again. Any other failure ends the wait.
func (m *ManagementTalos) wait(ctx context.Context, raw []byte, address, what string, do func(context.Context, TalosNode) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(m.StepTimeout, guestStepTimeout))
	defer cancel()
	poll := cmp.Or(m.Poll, talosRestorePoll)
	for {
		done := false
		err := m.with(ctx, raw, address, func(ctx context.Context, t TalosNode) error {
			var err error
			done, err = do(ctx, t)
			return err
		})
		switch {
		case err == nil && done:
			return nil
		case err != nil && !errors.Is(err, errTalosNotReady) && ctx.Err() == nil:
			return fmt.Errorf("%s: %s: %w", what, address, err)
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
