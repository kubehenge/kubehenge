package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubectl/pkg/drain"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// TalosUpgradeFailedAnnotation records why the last node upgrade failed: "<node> to <release> at
// <time>: <why>". While it is there no node of the cluster is upgraded; removing it tries again.
const TalosUpgradeFailedAnnotation = "kubehenge.dev/talos-upgrade-failed"

const (
	// talosDrainTimeout bounds the eviction of a node's pods before it reboots. A pod a disruption
	// budget holds past it is stopped by the reboot, as it would be by a power cut: one node is down
	// at a time, for a minute or two.
	talosDrainTimeout = 5 * time.Minute
	// talosInstallTimeout bounds pulling the installer and writing it to disk.
	talosInstallTimeout = 15 * time.Minute
	// talosBootTimeout is how long a rebooted node has to come up Ready on the new release. Talos
	// boots the release before when the new one fails to, so one Ready on the old release past
	// this has been rolled back.
	talosBootTimeout = 15 * time.Minute
)

// TalosImages are where Talos releases come from.
type TalosImages interface {
	TalosInstaller(ctx context.Context, version string) (string, error)
	TalosReleases(ctx context.Context) ([]string, error)
}

// NodeDrainer moves a workload cluster's pods off a node and lets them back.
type NodeDrainer interface {
	Drain(ctx context.Context, c *clusterv1.Cluster, node string) error
	Uncordon(ctx context.Context, c *clusterv1.Cluster, node string) error
}

// TalosUpgrader upgrades the nodes of Talos clusters in place to their talosVersion, one node at a
// time, the control plane first, through Talos's own upgrade: the installer of the release is
// written to the node's disk, the node is drained and reboots into it, and Talos boots the release
// before if the new one fails to come up. A node more than one minor version behind steps through
// the newest release of each minor version between. Only a settled cluster starts a node's
// upgrade, and BeforeClusterUpgrade holds a Kubernetes upgrade while one is under way.
type TalosUpgrader struct {
	Client client.Client
	// Reader reads the talosconfig Secrets, past any cache.
	Reader         client.Reader
	ClassNamespace string
	Images         TalosImages
	Dial           TalosDialer
	Nodes          NodeDrainer
	// Events reports each node's upgrade on its Cluster; may be nil.
	Events   events.EventRecorder
	Interval time.Duration
	Now      func() time.Time

	mu   sync.Mutex
	busy map[types.NamespacedName]bool
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: one replica upgrades.
func (*TalosUpgrader) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (u *TalosUpgrader) Start(ctx context.Context) error {
	t := time.NewTicker(u.Interval)
	defer t.Stop()
	var running sync.WaitGroup
	defer running.Wait()
	for {
		u.RunOnce(ctx, &running)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// RunOnce looks at every Talos cluster once. A node's upgrade runs on beside the next looks, which
// pass over its cluster until it is done; running counts the upgrades started.
func (u *TalosUpgrader) RunOnce(ctx context.Context, running *sync.WaitGroup) {
	log := ctrl.LoggerFrom(ctx).WithName("talos-upgrader")
	var list clusterv1.ClusterList
	if err := u.Client.List(ctx, &list); err != nil {
		log.Error(err, "listing clusters")
		return
	}
	for i := range list.Items {
		c := &list.Items[i]
		if !Owns(c, u.ClassNamespace) || !c.DeletionTimestamp.IsZero() || !isTalos(c) || u.isBusy(c) {
			continue
		}
		step, err := u.look(ctx, c)
		if err != nil {
			log.Error(err, "Talos upgrade", "cluster", client.ObjectKeyFromObject(c))
			continue
		}
		if step == nil {
			continue
		}
		u.markBusy(c)
		running.Go(func() {
			defer u.markDone(c)
			if err := u.upgrade(ctx, c, step); err != nil {
				log.Error(err, "Talos upgrade", "cluster", client.ObjectKeyFromObject(c), "node", step.node)
			}
		})
	}
}

func (u *TalosUpgrader) isBusy(c *clusterv1.Cluster) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.busy[client.ObjectKeyFromObject(c)]
}

func (u *TalosUpgrader) markBusy(c *clusterv1.Cluster) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy == nil {
		u.busy = map[types.NamespacedName]bool{}
	}
	u.busy[client.ObjectKeyFromObject(c)] = true
}

func (u *TalosUpgrader) markDone(c *clusterv1.Cluster) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.busy, client.ObjectKeyFromObject(c))
}

// talosProgress is a node upgrade under way, as TalosUpgradingAnnotation records it.
type talosProgress struct {
	node, release string
	since         time.Time
	// rebooted is when the node was told to reboot into the release; zero before.
	rebooted time.Time
}

var talosProgressFormat = regexp.MustCompile(`^(\S+) to (v\d+\.\d+\.\d+) since (\S+?)(?:, rebooted at (\S+))?$`)

func parseTalosProgress(s string) (talosProgress, bool) {
	m := talosProgressFormat.FindStringSubmatch(s)
	if m == nil {
		return talosProgress{}, false
	}
	p := talosProgress{node: m[1], release: m[2]}
	var err error
	if p.since, err = time.Parse(time.RFC3339, m[3]); err != nil {
		return talosProgress{}, false
	}
	if m[4] != "" {
		if p.rebooted, err = time.Parse(time.RFC3339, m[4]); err != nil {
			return talosProgress{}, false
		}
	}
	return p, true
}

func (p *talosProgress) String() string {
	s := fmt.Sprintf("%s to %s since %s", p.node, p.release, p.since.UTC().Format(time.RFC3339))
	if !p.rebooted.IsZero() {
		s += ", rebooted at " + p.rebooted.UTC().Format(time.RFC3339)
	}
	return s
}

// talosStep is a node's upgrade to start or carry on with.
type talosStep struct {
	talosProgress

	address string
}

var talosOSImage = regexp.MustCompile(`^Talos \((v\d+\.\d+\.\d+)\)$`)

// MachineTalos is the Talos release a machine's node runs, as its kubelet reports it.
func MachineTalos(m *clusterv1.Machine) (string, bool) {
	if m.Status.NodeInfo == nil {
		return "", false
	}
	v := talosOSImage.FindStringSubmatch(m.Status.NodeInfo.OSImage)
	if v == nil {
		return "", false
	}
	return v[1], true
}

func machineAddress(m *clusterv1.Machine) string {
	for _, a := range m.Status.Addresses {
		if a.Type == clusterv1.MachineInternalIP {
			return a.Address
		}
	}
	return ""
}

// look follows the node upgrade under way, and returns the one to start or carry on with.
func (u *TalosUpgrader) look(ctx context.Context, c *clusterv1.Cluster) (*talosStep, error) {
	machines, err := u.machines(ctx, c)
	if err != nil {
		return nil, err
	}
	if p, ok := parseTalosProgress(c.Annotations[TalosUpgradingAnnotation]); ok {
		return u.follow(ctx, c, machines, &p)
	}
	if _, failed := c.Annotations[TalosUpgradeFailedAnnotation]; failed || !settled(c) {
		return nil, nil //nolint:nilnil // no node to upgrade now is no error
	}
	target, err := TalosVersion(c)
	if err != nil {
		return nil, err
	}
	for i := range machines {
		m := &machines[i]
		running, ok := MachineTalos(m)
		if !ok || m.Status.NodeRef.Name == "" {
			// Not up yet: a settled cluster has none such for long.
			return nil, nil //nolint:nilnil // as above
		}
		if running == target {
			continue
		}
		release, err := u.stepTo(ctx, running, target)
		if err == nil {
			err = templates.TalosUpgrade(running, release)
		}
		p := talosProgress{node: m.Status.NodeRef.Name, release: target, since: u.Now()}
		if err != nil {
			return nil, u.fail(ctx, c, &p, err)
		}
		p.release = release
		return &talosStep{talosProgress: p, address: machineAddress(m)}, nil
	}
	return nil, nil //nolint:nilnil // every node runs the release
}

// stepTo is the release a node on running moves to next on its way to target.
func (u *TalosUpgrader) stepTo(ctx context.Context, running, target string) (string, error) {
	if step, err := templates.TalosStep(running, target, nil); err == nil {
		return step, nil
	}
	releases, err := u.Images.TalosReleases(ctx)
	if err != nil {
		return "", fmt.Errorf("stepping from %s to %s: %w", running, target, err)
	}
	return templates.TalosStep(running, target, releases) //nolint:wrapcheck // its errors name both releases
}

// machines are a cluster's machines, those of the control plane first, each group by name.
func (u *TalosUpgrader) machines(ctx context.Context, c *clusterv1.Cluster) ([]clusterv1.Machine, error) {
	var list clusterv1.MachineList
	if err := u.Client.List(ctx, &list, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing the machines of cluster %s/%s: %w", c.Namespace, c.Name, err)
	}
	slices.SortFunc(list.Items, func(a, b clusterv1.Machine) int {
		_, aCP := a.Labels[clusterv1.MachineControlPlaneLabel]
		_, bCP := b.Labels[clusterv1.MachineControlPlaneLabel]
		switch {
		case aCP && !bCP:
			return -1
		case bCP && !aCP:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return list.Items, nil
}

// follow carries a node upgrade under way on: one not rebooted yet is started again, after a
// restart of the controller, as each of its steps can be taken twice; one rebooted is done once
// the node is Ready on the release, and failed when it is not by talosBootTimeout.
func (u *TalosUpgrader) follow(ctx context.Context, c *clusterv1.Cluster, machines []clusterv1.Machine, p *talosProgress) (*talosStep, error) {
	i := slices.IndexFunc(machines, func(m clusterv1.Machine) bool { return m.Status.NodeRef.Name == p.node })
	if i < 0 {
		// Cluster API replaced the machine; its successor is looked at as any other.
		return nil, u.annotate(ctx, c, map[string]any{TalosUpgradingAnnotation: nil})
	}
	m := &machines[i]
	if p.rebooted.IsZero() {
		return &talosStep{talosProgress: *p, address: machineAddress(m)}, nil
	}
	running, _ := MachineTalos(m)
	ready := meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineNodeReadyCondition)
	now := u.Now()
	switch {
	case running == p.release && ready:
		if err := u.Nodes.Uncordon(ctx, c, p.node); err != nil {
			return nil, fmt.Errorf("after the upgrade of %s: %w", p.node, err)
		}
		u.eventf(c, corev1.EventTypeNormal, "TalosUpgraded", "node %s runs Talos %s", p.node, p.release)
		return nil, u.annotate(ctx, c, map[string]any{
			TalosUpgradingAnnotation: nil,
			TalosUpgradedAnnotation:  fmt.Sprintf("%s to %s at %s", p.node, p.release, now.UTC().Format(time.RFC3339)),
		})
	case now.Sub(p.rebooted) < talosBootTimeout:
		return nil, nil //nolint:nilnil // the node is still booting
	case ready:
		_ = u.Nodes.Uncordon(ctx, c, p.node)
		return nil, u.fail(ctx, c, p, fmt.Errorf("the node came back on Talos %s: Talos booted the release before, as the new one did not come up", running))
	}
	return nil, u.fail(ctx, c, p, fmt.Errorf("the node is not Ready %s after it rebooted", talosBootTimeout))
}

// upgrade writes the release's installer to the node's disk, drains it and reboots it.
func (u *TalosUpgrader) upgrade(ctx context.Context, c *clusterv1.Cluster, s *talosStep) error {
	if err := u.annotate(ctx, c, map[string]any{TalosUpgradingAnnotation: s.String()}); err != nil {
		return err
	}
	if s.address == "" {
		return u.fail(ctx, c, &s.talosProgress, errors.New("the machine has no internal address to reach Talos at"))
	}
	image, err := u.Images.TalosInstaller(ctx, s.release)
	if err != nil {
		return fmt.Errorf("the installer of Talos %s: %w", s.release, err)
	}
	raw, err := readTalosconfig(ctx, u.Reader, c.Namespace, c.Name)
	if err != nil {
		return err
	}
	node, err := u.Dial(ctx, raw, s.address)
	if err != nil {
		return err
	}
	defer func() { _ = node.Close() }()
	ictx, cancel := context.WithTimeout(ctx, talosInstallTimeout)
	err = node.Install(ictx, image)
	cancel()
	if err != nil {
		return u.fail(ctx, c, &s.talosProgress, err)
	}
	u.eventf(c, corev1.EventTypeNormal, "TalosUpgrading", "node %s installed Talos %s and reboots into it", s.node, s.release)
	if err := u.Nodes.Drain(ctx, c, s.node); err != nil {
		u.eventf(c, corev1.EventTypeWarning, "TalosDrain", "node %s reboots without all its pods moved: %v", s.node, err)
	}
	s.rebooted = u.Now()
	if err := u.annotate(ctx, c, map[string]any{TalosUpgradingAnnotation: s.String()}); err != nil {
		return err
	}
	if err := node.Reboot(ctx); err != nil {
		_ = u.Nodes.Uncordon(ctx, c, s.node)
		return u.fail(ctx, c, &s.talosProgress, err)
	}
	return nil
}

// fail records why a node's upgrade failed, which stops the cluster's upgrades.
func (u *TalosUpgrader) fail(ctx context.Context, c *clusterv1.Cluster, p *talosProgress, why error) error {
	u.eventf(c, corev1.EventTypeWarning, "TalosUpgradeFailed", "node %s to Talos %s: %v", p.node, p.release, why)
	return u.annotate(ctx, c, map[string]any{
		TalosUpgradingAnnotation:     nil,
		TalosUpgradeFailedAnnotation: fmt.Sprintf("%s to %s at %s: %v", p.node, p.release, u.Now().UTC().Format(time.RFC3339), why),
	})
}

// annotate sets annotations on the cluster, and removes those whose value is nil.
func (u *TalosUpgrader) annotate(ctx context.Context, c *clusterv1.Cluster, annotations map[string]any) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: annotations}})
	if err != nil {
		return fmt.Errorf("recording the Talos upgrade: %w", err)
	}
	target := &clusterv1.Cluster{}
	target.Namespace, target.Name = c.Namespace, c.Name
	if err := u.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("recording the Talos upgrade: %w", err)
	}
	return nil
}

func (u *TalosUpgrader) eventf(c *clusterv1.Cluster, kind, reason, format string, args ...any) {
	if u.Events != nil {
		u.Events.Eventf(c, nil, kind, reason, "TalosUpgrade", format, args...)
	}
}

// RemoteDrainer drains a workload cluster's nodes as kubectl drain does, through the kubeconfig
// Cluster API keeps for it: DaemonSets' pods stay, emptyDir data goes with its pod, and pods no
// controller owns are deleted, as the node's reboot would stop them anyway.
type RemoteDrainer struct {
	Remotes *Remotes
	Timeout time.Duration
}

// Drain implements NodeDrainer.
func (d *RemoteDrainer) Drain(ctx context.Context, c *clusterv1.Cluster, node string) error {
	h, err := d.helper(ctx, c)
	if err != nil {
		return err
	}
	n, err := h.Client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		d.Remotes.forget(c)
		return fmt.Errorf("reading node %s: %w", node, err)
	}
	if err := drain.RunCordonOrUncordon(h, n, true); err != nil {
		return fmt.Errorf("cordoning node %s: %w", node, err)
	}
	if err := drain.RunNodeDrain(h, node); err != nil {
		return fmt.Errorf("draining node %s: %w", node, err)
	}
	return nil
}

// Uncordon implements NodeDrainer.
func (d *RemoteDrainer) Uncordon(ctx context.Context, c *clusterv1.Cluster, node string) error {
	h, err := d.helper(ctx, c)
	if err != nil {
		return err
	}
	n, err := h.Client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		d.Remotes.forget(c)
		return fmt.Errorf("reading node %s: %w", node, err)
	}
	if err := drain.RunCordonOrUncordon(h, n, false); err != nil {
		return fmt.Errorf("uncordoning node %s: %w", node, err)
	}
	return nil
}

func (d *RemoteDrainer) helper(ctx context.Context, c *clusterv1.Cluster) (*drain.Helper, error) {
	r, found, err := d.Remotes.get(ctx, c)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the cluster has no kubeconfig yet")
	}
	cs, err := kubernetes.NewForConfig(r.config)
	if err != nil {
		return nil, fmt.Errorf("reaching cluster %s/%s: %w", c.Namespace, c.Name, err)
	}
	return &drain.Helper{
		Ctx: ctx, Client: cs, Force: true, IgnoreAllDaemonSets: true, DeleteEmptyDirData: true,
		GracePeriodSeconds: -1, Timeout: cmpDuration(d.Timeout, talosDrainTimeout),
		Out: io.Discard, ErrOut: io.Discard,
	}, nil
}

func cmpDuration(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}
