package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeTalos records what the upgrader and the restorer do on each node through the Talos API. Its
// nodes run etcd until wiped, boot again at once, and wait in Preparing until bootstrapped.
type fakeTalos struct {
	mu         sync.Mutex
	installed  map[string]string
	rebooted   []string
	installErr error

	// calls are the restore's calls, "<address> <call>".
	calls    []string
	boots    map[string]int
	wiped    map[string]bool
	uploaded map[string][]byte
	// down are the nodes whose API does not answer; fail decides which calls fail.
	down map[string]bool
	fail func(address, call string) error
}

// call records a restore's call, and fails it as the fake is told to.
func (f *fakeTalos) call(address, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[address] {
		return fmt.Errorf("%w: connection refused", errTalosNotReady)
	}
	f.calls = append(f.calls, address+" "+name)
	if f.fail != nil {
		return f.fail(address, name)
	}
	return nil
}

func (f *fakeTalos) restoreCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool {
		return strings.HasSuffix(c, " boot") || strings.HasSuffix(c, " etcd")
	})
}

//nolint:ireturn // a TalosDialer
func (f *fakeTalos) dial(_ context.Context, raw []byte, address string) (TalosNode, error) {
	if string(raw) != "talosconfig of web" {
		return nil, errors.New("dialled with another cluster's talosconfig")
	}
	return &fakeTalosNode{f: f, address: address}, nil
}

type fakeTalosNode struct {
	f       *fakeTalos
	address string
}

func (n *fakeTalosNode) Install(_ context.Context, image string) error {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	if n.f.installErr != nil {
		return n.f.installErr
	}
	if n.f.installed == nil {
		n.f.installed = map[string]string{}
	}
	n.f.installed[n.address] = image
	return nil
}

func (n *fakeTalosNode) Reboot(context.Context) error {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	n.f.rebooted = append(n.f.rebooted, n.address)
	return nil
}

func (*fakeTalosNode) EtcdSnapshot(context.Context) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("snapshot")), "3.6.5", nil
}

func (n *fakeTalosNode) BootID(context.Context) (string, error) {
	if err := n.f.call(n.address, "boot"); err != nil {
		return "", err
	}
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	return strconv.Itoa(n.f.boots[n.address]), nil
}

func (n *fakeTalosNode) WipeEtcd(context.Context) error {
	if err := n.f.call(n.address, "wipe"); err != nil {
		return err
	}
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	if n.f.boots == nil {
		n.f.boots, n.f.wiped = map[string]int{}, map[string]bool{}
	}
	n.f.boots[n.address]++
	n.f.wiped[n.address] = true
	delete(n.f.uploaded, n.address)
	return nil
}

func (n *fakeTalosNode) Etcd(context.Context) (string, bool, error) {
	if err := n.f.call(n.address, "etcd"); err != nil {
		return "", false, err
	}
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	if n.f.wiped[n.address] {
		return talosEtcdPreparing, false, nil
	}
	return talosEtcdRunning, true, nil
}

func (n *fakeTalosNode) UploadEtcdSnapshot(_ context.Context, snapshot io.Reader) error {
	data, err := io.ReadAll(snapshot)
	if err != nil {
		return err
	}
	if err := n.f.call(n.address, "upload"); err != nil {
		return err
	}
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	if n.f.uploaded == nil {
		n.f.uploaded = map[string][]byte{}
	}
	n.f.uploaded[n.address] = data
	return nil
}

func (n *fakeTalosNode) BootstrapRecovered(context.Context) error {
	if err := n.f.call(n.address, "bootstrap"); err != nil {
		return err
	}
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	if n.f.uploaded[n.address] == nil {
		return errors.New("no snapshot uploaded")
	}
	n.f.wiped[n.address] = false
	return nil
}

func (*fakeTalosNode) Close() error { return nil }

type fakeDrainer struct {
	mu                  sync.Mutex
	drained, uncordoned []string
}

func (d *fakeDrainer) Drain(_ context.Context, _ *clusterv1.Cluster, node string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.drained = append(d.drained, node)
	return nil
}

func (d *fakeDrainer) Uncordon(_ context.Context, _ *clusterv1.Cluster, node string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.uncordoned = append(d.uncordoned, node)
	return nil
}

type fakeTalosImages []string

func (fakeTalosImages) TalosInstaller(_ context.Context, version string) (string, error) {
	return "factory.talos.dev/nocloud-installer/abc:" + version, nil
}

func (f fakeTalosImages) TalosReleases(context.Context) ([]string, error) { return f, nil }

func talosCluster(name, talos string) *clusterv1.Cluster {
	c := settledCluster(name, "v1.36.5", map[string]string{TalosVersionVariable: `"` + talos + `"`})
	c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
	c.Annotations = map[string]string{}
	return c
}

// talosMachine is a Ready machine of cluster web on a Talos release; those named web-cp-* are of
// the control plane.
func talosMachine(name, address, release string) *clusterv1.Machine {
	m := &clusterv1.Machine{
		Namespace: "clusters", Name: name,
		Labels: map[string]string{clusterv1.ClusterNameLabel: "web"},
		Status: clusterv1.MachineStatus{
			NodeRef:    clusterv1.MachineNodeReference{Name: name},
			NodeInfo:   &corev1.NodeSystemInfo{OSImage: "Talos (" + release + ")"},
			Addresses:  clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: address}},
			Conditions: []metav1.Condition{{Type: clusterv1.MachineNodeReadyCondition, Status: metav1.ConditionTrue}},
		},
	}
	if strings.HasPrefix(name, "web-cp-") {
		m.Labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	return m
}

// talosRig is a TalosUpgrader over a fake API server, Talos API and drainer, on a clock the test
// moves.
type talosRig struct {
	u       *TalosUpgrader
	kube    client.Client
	talos   *fakeTalos
	drainer *fakeDrainer
	now     time.Time
}

func newTalosRig(t *testing.T, objects ...client.Object) *talosRig {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clusterv1.AddToScheme, clientgoscheme.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	secret := &corev1.Secret{Namespace: "clusters", Name: TalosconfigSecret("web"), Data: map[string][]byte{TalosconfigKey: []byte("talosconfig of web")}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, secret)...).Build()
	r := &talosRig{kube: kube, talos: &fakeTalos{}, drainer: &fakeDrainer{}, now: day(10)}
	r.u = &TalosUpgrader{
		Client: kube, Reader: kube, ClassNamespace: "kubehenge-system",
		Images: fakeTalosImages{"v1.13.9", "v1.13.12", "v1.14.4"}, Dial: r.talos.dial, Nodes: r.drainer,
		Now: func() time.Time { return r.now },
	}
	return r
}

// run looks at every cluster once, waits for the upgrades it starts, and returns cluster web.
func (r *talosRig) run(t *testing.T) *clusterv1.Cluster {
	t.Helper()
	var running sync.WaitGroup
	r.u.RunOnce(context.Background(), &running)
	running.Wait()
	c := &clusterv1.Cluster{}
	if err := r.kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	return c
}

// setMachine reports a machine's node on a Talos release, Ready or not.
func (r *talosRig) setMachine(t *testing.T, name, release string, ready metav1.ConditionStatus) {
	t.Helper()
	m := &clusterv1.Machine{}
	if err := r.kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: name}, m); err != nil {
		t.Fatal(err)
	}
	m.Status.NodeInfo.OSImage = "Talos (" + release + ")"
	m.Status.Conditions = []metav1.Condition{{Type: clusterv1.MachineNodeReadyCondition, Status: ready}}
	if err := r.kube.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func TestTalosUpgraderUpgradesOneNodeAtATimeControlPlaneFirst(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"),
		talosMachine("web-md-0", "192.0.2.20", "v1.14.3"),
		talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	c := r.run(t)
	p, ok := parseTalosProgress(c.Annotations[TalosUpgradingAnnotation])
	if !ok || p.node != "web-cp-0" || p.release != "v1.14.4" || p.rebooted.IsZero() {
		t.Fatalf("upgrading %q", c.Annotations[TalosUpgradingAnnotation])
	}
	if r.talos.installed["192.0.2.10"] != "factory.talos.dev/nocloud-installer/abc:v1.14.4" || len(r.talos.installed) != 1 ||
		!slices.Equal(r.talos.rebooted, []string{"192.0.2.10"}) || !slices.Equal(r.drainer.drained, []string{"web-cp-0"}) {
		t.Fatalf("installed %v, rebooted %v, drained %v", r.talos.installed, r.talos.rebooted, r.drainer.drained)
	}
	// Until the node is back on the release, nothing else starts.
	r.now = r.now.Add(time.Minute)
	r.setMachine(t, "web-cp-0", "v1.14.3", metav1.ConditionFalse)
	if c = r.run(t); c.Annotations[TalosUpgradingAnnotation] == "" || len(r.talos.rebooted) != 1 {
		t.Fatalf("moved on early: %v", c.Annotations)
	}
	r.setMachine(t, "web-cp-0", "v1.14.4", metav1.ConditionTrue)
	c = r.run(t)
	if _, upgrading := c.Annotations[TalosUpgradingAnnotation]; upgrading ||
		!strings.HasPrefix(c.Annotations[TalosUpgradedAnnotation], "web-cp-0 to v1.14.4 at ") ||
		!slices.Equal(r.drainer.uncordoned, []string{"web-cp-0"}) {
		t.Fatalf("after the node came back: %v, uncordoned %v", c.Annotations, r.drainer.uncordoned)
	}
	c = r.run(t)
	if p, _ := parseTalosProgress(c.Annotations[TalosUpgradingAnnotation]); p.node != "web-md-0" {
		t.Fatalf("the worker next: %v", c.Annotations)
	}
}

func TestTalosUpgraderStepsThroughEachMinor(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.12.6"))
	c := r.run(t)
	if p, _ := parseTalosProgress(c.Annotations[TalosUpgradingAnnotation]); p.release != "v1.13.12" ||
		r.talos.installed["192.0.2.10"] != "factory.talos.dev/nocloud-installer/abc:v1.13.12" {
		t.Fatalf("upgrading %q, installed %v", c.Annotations[TalosUpgradingAnnotation], r.talos.installed)
	}
}

func TestTalosUpgraderStopsAtAFailure(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.talos.installErr = errors.New("no space left on device")
	c := r.run(t)
	if !strings.Contains(c.Annotations[TalosUpgradeFailedAnnotation], "no space left on device") || len(r.talos.rebooted) != 0 {
		t.Fatalf("annotations %v, rebooted %v", c.Annotations, r.talos.rebooted)
	}
	r.talos.installErr = nil
	if c = r.run(t); c.Annotations[TalosUpgradingAnnotation] != "" {
		t.Fatalf("tried again by itself: %v", c.Annotations)
	}
}

func TestTalosUpgraderTakesARollbackForAFailure(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.run(t)
	r.now = r.now.Add(talosBootTimeout + time.Minute)
	c := r.run(t)
	if !strings.Contains(c.Annotations[TalosUpgradeFailedAnnotation], "came back on Talos v1.14.3") ||
		c.Annotations[TalosUpgradingAnnotation] != "" || !slices.Equal(r.drainer.uncordoned, []string{"web-cp-0"}) {
		t.Fatalf("annotations %v, uncordoned %v", c.Annotations, r.drainer.uncordoned)
	}
}

func TestTalosUpgraderCarriesOnAfterARestart(t *testing.T) {
	t.Parallel()
	c := talosCluster("web", "v1.14.4")
	c.Annotations[TalosUpgradingAnnotation] = "web-cp-0 to v1.14.4 since " + day(9).Format(time.RFC3339)
	// Not settled any more: the step under way goes on all the same.
	c.Status.Conditions = nil
	r := newTalosRig(t, c, talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.run(t)
	if !slices.Equal(r.talos.rebooted, []string{"192.0.2.10"}) {
		t.Fatalf("rebooted %v", r.talos.rebooted)
	}
}

func TestTalosUpgraderLeavesOtherClustersAlone(t *testing.T) {
	t.Parallel()
	rke2 := settledCluster("web", "v1.36.5+rke2r1", nil)
	r := newTalosRig(t, rke2, talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.run(t)
	if len(r.talos.installed) != 0 {
		t.Fatalf("installed %v", r.talos.installed)
	}
}

func TestTalosProgressReadsAsWritten(t *testing.T) {
	t.Parallel()
	p := talosProgress{node: "web-cp-0", release: "v1.14.4", since: day(1), rebooted: day(2)}
	got, ok := parseTalosProgress(p.String())
	if !ok || got.node != p.node || got.release != p.release || !got.since.Equal(p.since) || !got.rebooted.Equal(p.rebooted) {
		t.Fatalf("%q read as %+v", p.String(), got)
	}
	if _, ok := parseTalosProgress("web-cp-0 to 1.14 since yesterday"); ok {
		t.Error("read a malformed one")
	}
}

// After a node's upgrade, the DaemonSets' pods its shutdown rejected go, and nothing else does.
func TestClearRejectedDaemonPods(t *testing.T) {
	t.Parallel()
	pod := func(name, node, kind string, phase corev1.PodPhase) *corev1.Pod {
		p := &corev1.Pod{
			Namespace: "istio-node", Name: name,
			Spec:   corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{Phase: phase},
		}
		if kind != "" {
			yes := true
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: "owner", UID: "u", Controller: &yes}}
		}
		return p
	}
	cs := kubefake.NewClientset(
		pod("ztunnel-rejected-1", "web-w-1", "DaemonSet", corev1.PodFailed),
		pod("ztunnel-rejected-2", "web-w-1", "DaemonSet", corev1.PodFailed),
		pod("ztunnel-running", "web-w-1", "DaemonSet", corev1.PodRunning),
		pod("job-failed", "web-w-1", "Job", corev1.PodFailed),
		pod("bare-failed", "web-w-1", "", corev1.PodFailed),
		pod("ztunnel-elsewhere", "web-w-2", "DaemonSet", corev1.PodFailed),
	)
	if err := clearRejectedDaemonPods(t.Context(), cs, "web-w-1"); err != nil {
		t.Fatal(err)
	}
	pods, err := cs.CoreV1().Pods("istio-node").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	left := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		left = append(left, p.Name)
	}
	slices.Sort(left)
	if want := []string{"bare-failed", "job-failed", "ztunnel-elsewhere", "ztunnel-running"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}
