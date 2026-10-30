package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// talosAPIPort is where every Talos node serves its API.
const talosAPIPort = 50000

// TalosNode is what the controller does on one Talos node through its API.
type TalosNode interface {
	// Install writes a release's installer image to the node's disk, for its next boot.
	Install(ctx context.Context, image string) error
	// Reboot restarts the node.
	Reboot(ctx context.Context) error
	// EtcdSnapshot streams a snapshot of the etcd member on a control-plane node, with the
	// member's etcd release.
	EtcdSnapshot(ctx context.Context) (io.ReadCloser, string, error)
	// BootID tells one boot of the node from the next.
	BootID(ctx context.Context) (string, error)
	// WipeEtcd resets the node without leaving etcd first, wiping its EPHEMERAL partition and etcd's
	// data with it, and reboots it. Its machine configuration stays.
	WipeEtcd(ctx context.Context) error
	// Etcd is the state of the node's etcd service, as Talos names it, and whether it is healthy.
	Etcd(ctx context.Context) (string, bool, error)
	// UploadEtcdSnapshot hands the node the snapshot BootstrapRecovered restores etcd from.
	UploadEtcdSnapshot(ctx context.Context, snapshot io.Reader) error
	// BootstrapRecovered starts etcd on the node as a cluster of one from the uploaded snapshot.
	BootstrapRecovered(ctx context.Context) error
	Close() error
}

// errTalosNotReady is a node's refusal of a call it takes later in its boot.
var errTalosNotReady = errors.New("the node is not ready for it yet")

// notReady marks the errors of a node that is still booting, or not answering, as errTalosNotReady.
func notReady(err error) error {
	switch status.Code(err) { //nolint:exhaustive // every other code is a failure of its own
	case codes.FailedPrecondition, codes.Unavailable:
		return fmt.Errorf("%w: %w", errTalosNotReady, err)
	}
	return err
}

// TalosDialer reaches one Talos node at its address with a cluster's talosconfig.
type TalosDialer func(ctx context.Context, clientConfig []byte, address string) (TalosNode, error)

// TalosconfigSecret is the Secret CABPT keeps a Talos cluster's talosconfig in, under the key
// TalosconfigKey.
func TalosconfigSecret(cluster string) string { return cluster + "-talosconfig" }

// TalosconfigKey is the key of the talosconfig in TalosconfigSecret.
const TalosconfigKey = "talosconfig"

// readTalosconfig reads a Talos cluster's talosconfig.
func readTalosconfig(ctx context.Context, r client.Reader, namespace, cluster string) ([]byte, error) {
	s := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: TalosconfigSecret(cluster)}, s); err != nil {
		return nil, fmt.Errorf("reading the talosconfig of cluster %s/%s: %w", namespace, cluster, err)
	}
	data := s.Data[TalosconfigKey]
	if len(data) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no %s", namespace, s.Name, TalosconfigKey)
	}
	return data, nil
}

// DialTalos is the TalosDialer of the Talos API itself. The node is its own endpoint: Talos
// advises against reaching nodes through the control plane's virtual address, which moves while
// the node holding it reboots.
func DialTalos(ctx context.Context, raw []byte, address string) (TalosNode, error) { //nolint:ireturn // a TalosDialer, which tests stand in for with nodes of their own
	cfg, err := talosconfig.FromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("reading the talosconfig: %w", err)
	}
	c, err := talosclient.New(ctx, talosclient.WithConfig(cfg),
		talosclient.WithEndpoints(net.JoinHostPort(address, strconv.Itoa(talosAPIPort))))
	if err != nil {
		return nil, fmt.Errorf("reaching the Talos API of %s: %w", address, err)
	}
	return &talosNode{c: c, address: address}, nil
}

type talosNode struct {
	c       *talosclient.Client
	address string
}

// systemContainerd is the containerd Talos pulls its own images into, the installer's among them.
var systemContainerd = &common.ContainerdInstance{Driver: common.ContainerDriver_CRI, Namespace: common.ContainerdNamespace_NS_SYSTEM}

// talosStallTimeout is how long an install goes without a word from the node before it is given
// up as stalled. A node whose VM is gone cannot end the stream, and while data is unacknowledged
// TCP's keepalive does not notice either; the kernel gives up only after about 15 minutes.
const talosStallTimeout = 3 * time.Minute

// errTalosStalled ends an install the node stopped reporting on.
var errTalosStalled = fmt.Errorf("no word from the node for %s", talosStallTimeout)

// stallGuard is ctx ended with errTalosStalled once heard has not been called for d; stop ends it.
func stallGuard(ctx context.Context, d time.Duration) (context.Context, func(), func()) {
	sctx, cancel := context.WithCancelCause(ctx)
	t := time.AfterFunc(d, func() { cancel(errTalosStalled) })
	return sctx, func() { t.Reset(d) }, func() {
		t.Stop()
		cancel(nil)
	}
}

func (n *talosNode) Install(ctx context.Context, image string) error {
	sctx, heard, stop := stallGuard(ctx, talosStallTimeout)
	defer stop()
	err := n.pullAndInstall(sctx, image, heard)
	if cause := context.Cause(sctx); err != nil && errors.Is(cause, errTalosStalled) {
		return fmt.Errorf("%s: installing %s: %w", n.address, image, cause)
	}
	return err
}

// lastMessages are the installer's last few messages, which say why it failed.
func lastMessages(said []string) string {
	const kept = 5
	if len(said) > kept {
		said = said[len(said)-kept:]
	}
	return strings.Join(said, "; ")
}

func (n *talosNode) Reboot(ctx context.Context) error {
	if err := n.c.Reboot(ctx); err != nil {
		return fmt.Errorf("%s: rebooting: %w", n.address, err)
	}
	return nil
}

func (n *talosNode) EtcdSnapshot(ctx context.Context) (io.ReadCloser, string, error) {
	etcd, err := n.c.EtcdStatus(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("%s: etcd status: %w", n.address, err)
	}
	version := ""
	for _, m := range etcd.GetMessages() {
		version = cmpOr(version, m.GetMemberStatus().GetProtocolVersion())
	}
	r, err := n.c.EtcdSnapshot(ctx, &machine.EtcdSnapshotRequest{})
	if err != nil {
		return nil, "", fmt.Errorf("%s: etcd snapshot: %w", n.address, err)
	}
	return r, version, nil
}

func (n *talosNode) BootID(ctx context.Context) (string, error) {
	r, err := n.c.Read(ctx, "/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("%s: reading the boot ID: %w", n.address, notReady(err))
	}
	defer func() { _ = r.Close() }()
	id, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("%s: reading the boot ID: %w", n.address, notReady(err))
	}
	return strings.TrimSpace(string(id)), nil
}

func (n *talosNode) WipeEtcd(ctx context.Context) error {
	err := n.c.ResetGeneric(ctx, &machine.ResetRequest{
		Reboot:                 true,
		SystemPartitionsToWipe: []*machine.ResetPartitionSpec{{Label: constants.EphemeralPartitionLabel, Wipe: true}},
	})
	if err != nil {
		return fmt.Errorf("%s: wiping etcd: %w", n.address, notReady(err))
	}
	return nil
}

func (n *talosNode) Etcd(ctx context.Context) (string, bool, error) {
	services, err := n.c.ServiceInfo(ctx, "etcd")
	if err != nil {
		return "", false, fmt.Errorf("%s: reading etcd's state: %w", n.address, notReady(err))
	}
	for _, s := range services {
		return s.Service.GetState(), s.Service.GetHealth().GetHealthy(), nil
	}
	return "", false, fmt.Errorf("%s: %w: etcd is not among its services", n.address, errTalosNotReady)
}

func (n *talosNode) UploadEtcdSnapshot(ctx context.Context, snapshot io.Reader) error {
	if _, err := n.c.EtcdRecover(ctx, snapshot); err != nil {
		return fmt.Errorf("%s: uploading the snapshot: %w", n.address, notReady(err))
	}
	return nil
}

func (n *talosNode) BootstrapRecovered(ctx context.Context) error {
	if err := n.c.Bootstrap(ctx, &machine.BootstrapRequest{RecoverEtcd: true}); err != nil {
		return fmt.Errorf("%s: restoring etcd from the snapshot: %w", n.address, notReady(err))
	}
	return nil
}

// talosSnapshot streams a snapshot through the Talos API of the first control-plane node that
// answers. Talos does not say the store's revision, which the snapshot is recorded without.
func (d *EtcdDialer) talosSnapshot(ctx context.Context, cluster *clusterv1.Cluster) (*EtcdStream, error) {
	if d.Talos == nil {
		return nil, errors.New("this controller does not reach Talos nodes")
	}
	raw, err := readTalosconfig(ctx, d.Reader, cluster.Namespace, cluster.Name)
	if err != nil {
		return nil, err
	}
	var machines clusterv1.MachineList
	err = d.Reader.List(ctx, &machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return nil, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	var errs []error
	for i := range machines.Items {
		address := machineAddress(&machines.Items[i])
		if address == "" || !machines.Items[i].DeletionTimestamp.IsZero() {
			continue
		}
		node, err := d.Talos(ctx, raw, address)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r, version, err := node.EtcdSnapshot(ctx)
		if err != nil {
			_ = node.Close()
			errs = append(errs, err)
			continue
		}
		return &EtcdStream{ReadCloser: closeBoth{r, node}, Version: version}, nil
	}
	if len(errs) == 0 {
		return nil, errors.New("no control-plane machine of the cluster has an address yet")
	}
	return nil, errors.Join(errs...)
}

// closeBoth closes the snapshot's stream, then the connection it came over.
type closeBoth struct {
	io.ReadCloser

	conn io.Closer
}

func (c closeBoth) Close() error {
	return errors.Join(c.ReadCloser.Close(), c.conn.Close())
}

func (n *talosNode) Close() error {
	if err := n.c.Close(); err != nil {
		return fmt.Errorf("closing the Talos API connection to %s: %w", n.address, err)
	}
	return nil
}

func (n *talosNode) pullAndInstall(ctx context.Context, image string, heard func()) error {
	pull, err := n.c.ImageClient.Pull(ctx, &machine.ImageServicePullRequest{Containerd: systemContainerd, ImageRef: image})
	if err != nil {
		return fmt.Errorf("%s: pulling %s: %w", n.address, image, err)
	}
	name := ""
	for {
		resp, recvErr := pull.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("%s: pulling %s: %w", n.address, image, recvErr)
		}
		heard()
		if got := resp.GetName(); got != "" {
			name = got
		}
	}
	if name == "" {
		return fmt.Errorf("%s: pulling %s named no image", n.address, image)
	}
	upgrade, err := n.c.LifecycleClient.Upgrade(ctx, &machine.LifecycleServiceUpgradeRequest{
		Containerd: systemContainerd, Source: &machine.InstallArtifactsSource{ImageName: name},
	})
	if err != nil {
		return fmt.Errorf("%s: installing %s: %w", n.address, image, err)
	}
	var said []string
	for {
		resp, err := upgrade.Recv()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: installing %s ended without an exit code: %s", n.address, image, lastMessages(said))
		}
		if err != nil {
			return fmt.Errorf("%s: installing %s: %w", n.address, image, err)
		}
		heard()
		switch p := resp.GetProgress().GetResponse().(type) {
		case *machine.LifecycleServiceInstallProgress_Message:
			said = append(said, p.Message)
		case *machine.LifecycleServiceInstallProgress_ExitCode:
			if p.ExitCode != 0 {
				return fmt.Errorf("%s: installing %s exited with %d: %s", n.address, image, p.ExitCode, lastMessages(said))
			}
			return nil
		}
	}
}
