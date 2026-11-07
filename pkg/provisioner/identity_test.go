package provisioner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// fakeWorkloads hands out one fake workload cluster.
type fakeWorkloads struct {
	c      client.Client
	err    error
	asked  int
	forgot int
}

func (f *fakeWorkloads) Client(context.Context, *clusterv1.Cluster) (client.Client, bool, error) {
	f.asked++
	if f.err != nil {
		return nil, false, f.err
	}
	return f.c, true, nil
}

func (f *fakeWorkloads) forget(*clusterv1.Cluster) { f.forgot++ }

// workloadFixture is the workload cluster of newVMFixture's: a node and a CSI node pod for each of the
// three joined machines, the nodes labelled with the Proxmox VE node of their VMs.
func workloadFixture(t *testing.T) *fakeWorkloads {
	t.Helper()
	return workloadWith(t, &interceptor.Funcs{})
}

// workloadWith is workloadFixture with funcs standing in for calls to the workload cluster's API.
func workloadWith(t *testing.T, funcs *interceptor.Funcs) *fakeWorkloads {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, 9)
	for i, zone := range []string{"pve1", "pve2", "pve3"} {
		name := "web-cp-" + string(rune('0'+i))
		objs = append(objs,
			&corev1.Node{Name: name, Labels: map[string]string{
				corev1.LabelTopologyZone: zone, corev1.LabelFailureDomainBetaZone: zone,
			}},
			&corev1.Pod{
				Namespace: metav1.NamespaceSystem, Name: "csi-node-" + name,
				Labels: map[string]string{"app.kubernetes.io/name": csiPluginName, "app.kubernetes.io/component": csiNodeComponent},
				Spec:   corev1.PodSpec{NodeName: name},
			},
			&corev1.Pod{
				Namespace: metav1.NamespaceSystem, Name: "csi-controller-" + name,
				Labels: map[string]string{"app.kubernetes.io/name": csiPluginName, "app.kubernetes.io/component": "controller"},
				Spec:   corev1.PodSpec{NodeName: name},
			},
		)
	}
	return &fakeWorkloads{c: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(*funcs).Build()}
}

// tick has the watch look once, and returns what the look returned.
func (f *vmFixture) tick(after time.Duration) error {
	f.t.Helper()
	f.now = f.now.Add(after)
	ctx := context.Background()
	look, err := f.watch.read(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(ctx, webKey, c); err != nil {
		f.t.Fatal(err)
	}
	return f.watch.watch(ctx, c, look)
}

func (f *vmFixture) drain() []string {
	var all []string
	for e := f.event(); e != ""; e = f.event() {
		all = append(all, e)
	}
	return all
}

func (f *fakeWorkloads) zone(t *testing.T, node string) string {
	t.Helper()
	n := &corev1.Node{}
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	if n.Labels[corev1.LabelTopologyZone] != n.Labels[corev1.LabelFailureDomainBetaZone] {
		t.Errorf("node %s: zone %q but beta zone %q", node, n.Labels[corev1.LabelTopologyZone], n.Labels[corev1.LabelFailureDomainBetaZone])
	}
	return n.Labels[corev1.LabelTopologyZone]
}

func (f *fakeWorkloads) hasPod(t *testing.T, name string) bool {
	t.Helper()
	err := f.c.Get(context.Background(), client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: name}, &corev1.Pod{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestVMWatchRenamesARenamedVMBack(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.names[102] = "cmdb-host-07"
	f.render()
	if err := f.tick(0); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/nodes/pve2/qemu/102/config map[name:web-cp-1]"}; !slices.Equal(f.edit.puts, want) {
		t.Fatalf("puts %v, want %v", f.edit.puts, want)
	}
	if f.names[102] != "web-cp-1" {
		t.Errorf("name %q", f.names[102])
	}
	events := f.drain()
	if len(events) != 1 || !strings.HasPrefix(events[0], `Normal VMRenamedBack VM 102 was renamed from "cmdb-host-07" back to "web-cp-1".`) ||
		!strings.Contains(events[0], "deletes the node") {
		t.Errorf("events %q", events)
	}
	if err := f.tick(15 * time.Second); err != nil || len(f.edit.puts) != 1 || len(f.drain()) != 0 {
		t.Errorf("a name that is right again was written again: %v %v", err, f.edit.puts)
	}
}

func TestVMWatchLeavesVMsWithTheirNameAlone(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	for range 3 {
		if err := f.tick(15 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.edit.puts) != 0 || len(f.drain()) != 0 {
		t.Errorf("puts %v", f.edit.puts)
	}
}

func TestVMWatchNeverRenamesAVMOutsideTheClustersPool(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.names[102], f.pools[102] = "someone-elses", "kubehenge/team-b.web"
	f.names[103], f.pools[103] = "no-pool-at-all", ""
	f.render()
	if err := f.tick(0); err != nil {
		t.Fatal(err)
	}
	if len(f.edit.puts) != 0 || len(f.drain()) != 0 {
		t.Errorf("puts %v", f.edit.puts)
	}
}

func TestVMWatchLeavesALockedVMToTheNextLook(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.names[102] = "cmdb-host-07"
	f.vms(map[int64]string{101: "running", 102: "running:backup", 103: "running", 104: "running"})
	if err := f.tick(0); err != nil || len(f.edit.puts) != 0 {
		t.Fatalf("a locked VM: %v, puts %v", err, f.edit.puts)
	}
	f.vms(map[int64]string{101: "running", 102: "running", 103: "running", 104: "running"})
	if err := f.tick(15 * time.Second); err != nil || f.names[102] != "web-cp-1" {
		t.Fatalf("name %q, %v", f.names[102], err)
	}
}

func TestVMWatchReportsAFailedRenameOnceAndRetriesIt(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.names[102] = "cmdb-host-07"
	f.render()
	f.edit.putErr = errors.New("403 Permission check failed (VM.Config.Options)")
	for range 3 {
		err := f.tick(15 * time.Second)
		if err == nil || !strings.Contains(err.Error(), "renaming VM 102 back to web-cp-1") {
			t.Fatalf("error %v", err)
		}
	}
	if len(f.edit.puts) != 3 {
		t.Errorf("tried %d times", len(f.edit.puts))
	}
	events := f.drain()
	if len(events) != 1 || !strings.HasPrefix(events[0], `Warning VMRenameFailed VM 102 is named "cmdb-host-07", not "web-cp-1", and renaming it back failed: 403 Permission check failed`) {
		t.Fatalf("events %q", events)
	}
	f.edit.putErr = nil
	if err := f.tick(15 * time.Second); err != nil || f.names[102] != "web-cp-1" {
		t.Fatalf("name %q, %v", f.names[102], err)
	}
	if events := f.drain(); len(events) != 1 || !strings.HasPrefix(events[0], "Normal VMRenamedBack") {
		t.Errorf("events %q", events)
	}
}

func TestVMWatchRenamesOneVMWhenAnotherFails(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.names[101], f.names[103] = "first", "third"
	f.render()
	failing := &failingFirst{editingProxmox: f.edit}
	f.watch.Proxmox = failing
	if err := f.tick(0); err == nil {
		t.Fatal("no error")
	}
	if f.names[101] != "first" || f.names[103] != "web-cp-2" {
		t.Errorf("names %q %q", f.names[101], f.names[103])
	}
}

// failingFirst refuses the first rename it is asked for.
type failingFirst struct {
	*editingProxmox

	refused bool
}

func (p *failingFirst) Put(ctx context.Context, path string, params, out any) error {
	if !p.refused {
		p.refused = true
		return errors.New("500 busy")
	}
	return p.editingProxmox.Put(ctx, path, params, out)
}

func TestVMWatchFollowsAMovedVMWithItsNodesZone(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	w := workloadFixture(t)
	f.watch.Workloads = w
	if err := f.tick(0); err != nil {
		t.Fatal(err)
	}
	if w.asked != 1 {
		t.Fatalf("the first look asked the cluster %d times", w.asked)
	}
	if events := f.drain(); len(events) != 0 {
		t.Fatalf("events %q", events)
	}
	asked := w.asked
	if err := f.tick(15 * time.Second); err != nil || w.asked != asked {
		t.Fatalf("a cluster whose VMs stay put was asked again: %v, %d", err, w.asked)
	}

	f.vmNodes[102] = "pve3"
	f.render()
	if err := f.tick(15 * time.Second); err != nil {
		t.Fatal(err)
	}
	if got := w.zone(t, "web-cp-1"); got != "pve3" {
		t.Errorf("zone %q", got)
	}
	if got := w.zone(t, "web-cp-0"); got != "pve1" {
		t.Errorf("an unmoved node's zone %q", got)
	}
	if w.hasPod(t, "csi-node-web-cp-1") {
		t.Error("the CSI node pod of the moved node was kept")
	}
	for _, kept := range []string{"csi-node-web-cp-0", "csi-node-web-cp-2", "csi-controller-web-cp-1"} {
		if !w.hasPod(t, kept) {
			t.Errorf("pod %s was deleted", kept)
		}
	}
	events := f.drain()
	if len(events) != 1 || !strings.HasPrefix(events[0], "Normal NodeZoneUpdated Node web-cp-1 moved from Proxmox VE node pve2 to pve3.") {
		t.Errorf("events %q", events)
	}
	asked = w.asked
	if err := f.tick(15 * time.Second); err != nil || w.asked != asked {
		t.Errorf("the moved node was looked at again: %v, %d", err, w.asked)
	}
}

func TestVMWatchLeavesAnUnmovedVMsNodeAlone(t *testing.T) {
	t.Parallel()
	var writes []string
	w := workloadWith(t, &interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			writes = append(writes, "patch "+obj.GetName())
			return nil
		},
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			writes = append(writes, "delete "+obj.GetName())
			return nil
		},
	})
	f := newVMFixture(t)
	f.watch.Workloads = w
	for range 3 {
		if err := f.tick(15 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if len(writes) != 0 {
		t.Errorf("writes %v", writes)
	}
}

func TestVMWatchLeavesANodeWithoutAZoneToTheCloudControllerManager(t *testing.T) {
	t.Parallel()
	w := workloadFixture(t)
	node := &corev1.Node{}
	if err := w.c.Get(context.Background(), client.ObjectKey{Name: "web-cp-1"}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels = nil
	if err := w.c.Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	f := newVMFixture(t)
	f.watch.Workloads = w
	f.vmNodes[102] = "pve3"
	f.render()
	if err := f.tick(0); err != nil {
		t.Fatal(err)
	}
	if got := w.zone(t, "web-cp-1"); got != "" {
		t.Errorf("zone %q", got)
	}
	if !w.hasPod(t, "csi-node-web-cp-1") {
		t.Error("the plugin was restarted though nothing changed")
	}
}

func TestVMWatchNeverTouchesTheNodesOfAVMOutsideThePool(t *testing.T) {
	t.Parallel()
	w := workloadFixture(t)
	f := newVMFixture(t)
	f.watch.Workloads = w
	f.vmNodes[102], f.pools[102] = "pve3", "kubehenge/team-b.web"
	f.render()
	if err := f.tick(0); err != nil {
		t.Fatal(err)
	}
	if got := w.zone(t, "web-cp-1"); got != "pve2" || !w.hasPod(t, "csi-node-web-cp-1") {
		t.Errorf("zone %q", got)
	}
}

func TestVMWatchRetriesAnUnreachableWorkloadClusterWithoutBlockingTheRest(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	w := workloadFixture(t)
	w.err = errors.New("dial tcp: i/o timeout")
	f.watch.Workloads = w
	f.vmNodes[102] = "pve3"
	f.names[103] = "renamed"
	f.render()
	err := f.tick(0)
	if err == nil || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("error %v", err)
	}
	if f.names[103] != "web-cp-2" {
		t.Errorf("the rename waited for the workload cluster: %q", f.names[103])
	}
	f.vms(map[int64]string{101: "stopped", 102: "running", 103: "running", 104: "running"})
	_ = f.tick(0)
	_ = f.tick(vmStateGrace)
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, VMsCondition); cond == nil || cond.Reason != VMsStopped {
		t.Errorf("the look at the VMs' states stopped with the workload cluster: %+v", cond)
	}
	// An unreachable cluster costs one attempt a look and makes the next one build a fresh client.
	asked := w.asked
	if err := f.tick(15 * time.Second); err == nil || w.asked != asked+1 {
		t.Errorf("error %v, asked %d", err, w.asked)
	}
	w.err = nil
	if err := f.tick(15 * time.Second); err != nil {
		t.Fatal(err)
	}
	if got := w.zone(t, "web-cp-1"); got != "pve3" {
		t.Errorf("zone %q after the cluster answered", got)
	}
}

func TestVMWatchRetriesAFailedPluginRestart(t *testing.T) {
	t.Parallel()
	failing := true
	w := workloadWith(t, &interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if failing {
				return errors.New("500 unavailable")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	f := newVMFixture(t)
	f.watch.Workloads = w
	f.vmNodes[102] = "pve3"
	f.render()
	if err := f.tick(0); err == nil || !strings.Contains(err.Error(), "restarting the CSI node pod csi-node-web-cp-1") {
		t.Fatalf("error %v", err)
	}
	if got := w.zone(t, "web-cp-1"); got != "pve3" {
		t.Errorf("zone %q", got)
	}
	if w.forgot != 1 {
		t.Errorf("the failing client was kept: forgot %d", w.forgot)
	}
	events := f.drain()
	if len(events) != 2 || !strings.HasPrefix(events[0], "Normal NodeZoneUpdated") || !strings.HasPrefix(events[1], "Warning NodeZoneNotUpdated Node web-cp-1 is labelled zone pve3, but restarting its CSI node plugin failed") {
		t.Fatalf("events %q", events)
	}
	failing = false
	if err := f.tick(15 * time.Second); err != nil {
		t.Fatal(err)
	}
	if w.hasPod(t, "csi-node-web-cp-1") {
		t.Error("the pod was not restarted on the retry")
	}
	if events := f.drain(); len(events) != 0 {
		t.Errorf("the label was announced again: %q", events)
	}
	asked := w.asked
	if err := f.tick(15 * time.Second); err != nil || w.asked != asked {
		t.Errorf("settled node looked at again: %v", err)
	}
}
