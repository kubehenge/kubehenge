package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeProxmox answers each path with the JSON it holds.
type fakeProxmox map[string]string

func (f fakeProxmox) Get(_ context.Context, path string, v any) error {
	raw, ok := f[path]
	if !ok {
		return fmt.Errorf("%s: 404 Not Found", path)
	}
	return json.Unmarshal([]byte(raw), v)
}

type vmFixture struct {
	t      *testing.T
	pve    fakeProxmox
	kube   client.Client
	events *events.FakeRecorder
	watch  *VMWatch
	now    time.Time
}

// newVMFixture is web on local-lvm with three joined control planes, VMs 101 to 103 on pve1 to
// pve3, and a worker whose node has not joined, VM 104, all running.
func newVMFixture(t *testing.T) *vmFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	listGVK := proxmoxMachineGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	const machines = 4
	objs := make([]client.Object, 0, 1+2*machines)
	objs = append(objs, cluster(variableOf("storage", `"local-lvm"`)))
	for i, n := range []string{"pve1", "pve2", "pve3", "pve1"} {
		m, pm := controlPlane(fmt.Sprintf("web-cp-%d", i), int64(101+i), n)
		if i == 3 {
			m.Name, m.Status.NodeRef = "web-md-0", clusterv1.MachineNodeReference{}
		}
		pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web"})
		objs = append(objs, m, pm)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&clusterv1.Cluster{}, &clusterv1.Machine{}, proxmoxClusterObject()).Build()
	f := &vmFixture{t: t, pve: fakeProxmox{}, kube: kube, events: events.NewFakeRecorder(20), now: certNow}
	f.watch = &VMWatch{Client: kube, ClassNamespace: "kubehenge-system", Proxmox: f.pve, Events: f.events, Now: func() time.Time { return f.now }}
	f.vms(map[int64]string{101: "running", 102: "running", 103: "running", 104: "running"})
	f.storages(`[{"storage":"local-lvm","node":"pve1","status":"available","disk":10737418240,"maxdisk":107374182400}]`)
	f.nodes(nil)
	return f
}

// nodes sets which of pve1 to pve4 are offline in the resource list and which the HA manager has in
// maintenance. No VM of web runs on pve4.
func (f *vmFixture) nodes(offline []string, maintenance ...string) {
	names := []string{"pve1", "pve2", "pve3", "pve4"}
	listed, states := make([]string, 0, len(names)), make([]string, 0, len(names))
	for _, n := range names {
		status, ha := "online", "online"
		if slices.Contains(offline, n) {
			status, ha = "offline", "unknown"
		}
		if slices.Contains(maintenance, n) {
			ha = "maintenance"
		}
		listed = append(listed, fmt.Sprintf(`{"node":%q,"type":"node","status":%q}`, n, status))
		states = append(states, fmt.Sprintf("%q:%q", n, ha))
	}
	f.pve["/cluster/resources?type=node"] = "[" + strings.Join(listed, ",") + "]"
	f.pve["/cluster/ha/status/manager_status"] = `{"manager_status":{"node_status":{` + strings.Join(states, ",") + `}}}`
}

// machine reads one of web's machines.
func (f *vmFixture) machine(name string) *clusterv1.Machine {
	f.t.Helper()
	m := &clusterv1.Machine{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

// nodeReady has the Kubernetes node of one of web's machines Ready, and nodeNotReady not.
func (f *vmFixture) nodeReady(name string) { f.t.Helper(); f.setNodeReady(name, metav1.ConditionTrue) }

func (f *vmFixture) nodeNotReady() {
	f.t.Helper()
	f.setNodeReady(heldSecond, metav1.ConditionFalse)
}

func (f *vmFixture) setNodeReady(name string, status metav1.ConditionStatus) {
	f.t.Helper()
	m := f.machine(name)
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: clusterv1.MachineNodeReadyCondition, Status: status, Reason: "Test"})
	if err := f.kube.Status().Update(context.Background(), m); err != nil {
		f.t.Fatal(err)
	}
}

// held tells whether Cluster API is kept from replacing the machine, with Kubehenge's own record
// of it.
func (f *vmFixture) held(name string) bool {
	f.t.Helper()
	a := f.machine(name).Annotations
	_, skip := a[clusterv1.MachineSkipRemediationAnnotation]
	_, why := a[HoldAnnotation]
	if skip != why {
		f.t.Fatalf("%s: skip-remediation %v but Kubehenge's record %v", name, skip, why)
	}
	return skip
}

// vms sets each VM's status in the resource list, which for a started VM is QEMU's state
// ("running", "paused", "io-error"), with an optional lock after a colon.
func (f *vmFixture) vms(states map[int64]string) {
	nodes := map[int64]string{101: "pve1", 102: "pve2", 103: "pve3", 104: "pve1"}
	guests := make([]string, 0, len(states)+1)
	for vmid, entry := range states {
		status, lock, _ := strings.Cut(entry, ":")
		guests = append(guests, fmt.Sprintf(`{"vmid":%d,"node":%q,"type":"qemu","status":%q,"lock":%q}`, vmid, nodes[vmid], status, lock))
	}
	guests = append(guests, `{"vmid":200,"node":"pve1","type":"lxc","status":"stopped"}`)
	f.pve["/cluster/resources?type=vm"] = "[" + strings.Join(guests, ",") + "]"
}

func (f *vmFixture) storages(raw string) { f.pve["/cluster/resources?type=storage"] = raw }

// look has the watch look once, after, and returns the cluster's condition of that type.
func (f *vmFixture) look(after time.Duration, condType string) *metav1.Condition {
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
	if err := f.watch.watch(ctx, c, look); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.Get(ctx, webKey, c); err != nil {
		f.t.Fatal(err)
	}
	return meta.FindStatusCondition(c.Status.Conditions, condType)
}

func (f *vmFixture) event() string {
	select {
	case e := <-f.events.Events:
		return e
	default:
		return ""
	}
}

func TestVMWatchSaysWhichVMsDoNotRun(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	if cond := f.look(0, VMsCondition); cond != nil {
		t.Fatalf("all running: %+v", cond)
	}
	f.vms(map[int64]string{101: "io-error", 102: "paused", 103: "stopped", 104: "stopped"})
	if cond := f.look(time.Minute, VMsCondition); cond != nil {
		t.Fatalf("within the grace: %+v", cond)
	}
	cond := f.look(vmStateGrace, VMsCondition)
	if cond == nil || cond.Reason != VMsIOError {
		t.Fatalf("condition %+v", cond)
	}
	want := "web-cp-0 (VM 101 on pve1) paused: disk error; web-cp-1 (VM 102 on pve2) paused; web-cp-2 (VM 103 on pve3) stopped"
	if MessageHead(cond.Message) != want {
		t.Errorf("head %q\nwant %q", MessageHead(cond.Message), want)
	}
	for _, advice := range []string{vmAdvice[VMsIOError], vmAdvice[VMsStopped], vmAdvice[VMsPaused]} {
		if !strings.Contains(cond.Message, advice) {
			t.Errorf("message %q lacks %q", cond.Message, advice)
		}
	}
	if e := f.event(); !strings.HasPrefix(e, "Warning VMsNotRunning web-cp-0 (VM 101") {
		t.Errorf("event %q", e)
	}
	f.vms(map[int64]string{101: "running", 102: "running", 103: "running", 104: "stopped"})
	if again := f.look(time.Minute, VMsCondition); again != nil {
		t.Fatalf("all running again: %+v", again)
	}
	if e := f.event(); e != "Normal VMsRunning every VM of the cluster runs" {
		t.Errorf("event %q", e)
	}
	f.vms(map[int64]string{101: "running", 102: "running", 103: "guest-panicked", 104: "stopped"})
	f.look(time.Minute, VMsCondition)
	cond = f.look(vmStateGrace, VMsCondition)
	if cond == nil || cond.Reason != VMsPanicked || MessageHead(cond.Message) != "web-cp-2 (VM 103 on pve3) crashed" {
		t.Fatalf("crashed: %+v", cond)
	}
}

// Looked at every 15 seconds, a paused VM is reported at the first look past its grace, counted from
// the look that first found it paused, and the report clears at the first look after it runs.
func TestVMWatchReportsOnTheLookItsGraceEnds(t *testing.T) {
	t.Parallel()
	const every = 15 * time.Second
	f := newVMFixture(t)
	f.vms(map[int64]string{101: "running", 102: "paused", 103: "running", 104: "running"})
	for waited := time.Duration(0); waited < vmStateGrace; waited += every {
		if cond := f.look(every, VMsCondition); cond != nil {
			t.Fatalf("reported %v after the pause was found: %+v", waited, cond)
		}
	}
	if cond := f.look(every, VMsCondition); cond == nil || cond.Reason != VMsPaused {
		t.Fatalf("not reported on the look past the grace: %+v", cond)
	}
	f.vms(map[int64]string{101: "running", 102: "running", 103: "running", 104: "running"})
	if cond := f.look(every, VMsCondition); cond != nil {
		t.Fatalf("still reported a look after the VM runs: %+v", cond)
	}
}

// What Proxmox VE does on purpose, and a moment's pause, is not reported.
func TestVMWatchLetsPassingStatesBe(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.vms(map[int64]string{101: "stopped:backup", 102: "paused", 103: "running", 104: "running"})
	f.look(0, VMsCondition)
	f.vms(map[int64]string{101: "stopped:backup", 102: "running", 103: "postmigrate", 104: "running"})
	f.look(2*time.Minute, VMsCondition)
	f.vms(map[int64]string{101: "stopped:backup", 102: "paused", 103: "running", 104: "running"})
	if cond := f.look(2*time.Minute, VMsCondition); cond != nil {
		t.Fatalf("condition %+v", cond)
	}
	f.vms(map[int64]string{101: "stopped:backup", 102: "unknown", 103: "running", 104: "running"})
	f.look(0, VMsCondition)
	if cond := f.look(vmStateGrace, VMsCondition); cond != nil {
		t.Fatalf("a node that is offline: %+v", cond)
	}
}

func TestStorageNearlyFull(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	if cond := f.look(0, StorageCondition); cond != nil {
		t.Fatalf("10%% full: %+v", cond)
	}
	f.storages(`[
		{"storage":"local-lvm","node":"pve1","status":"available","disk":96636764160,"maxdisk":107374182400},
		{"storage":"local-lvm","node":"pve2","status":"available","disk":10737418240,"maxdisk":107374182400},
		{"storage":"local-lvm","node":"pve9","status":"available","disk":107374182400,"maxdisk":107374182400},
		{"storage":"local-lvm","node":"pve3","status":"available","disk":0,"maxdisk":0},
		{"storage":"ceph","node":"pve1","status":"available","shared":1,"disk":107374182400,"maxdisk":107374182400}
	]`)
	cond := f.look(time.Minute, StorageCondition)
	if cond == nil || cond.Reason != StorageNearlyFull || MessageHead(cond.Message) != "Storage local-lvm is 90% full on pve1 (90 of 100 GiB)" {
		t.Fatalf("condition %+v", cond)
	}
	if e := f.event(); !strings.HasPrefix(e, "Warning StorageNearlyFull Storage local-lvm is 90% full") {
		t.Errorf("event %q", e)
	}
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.Variables = []clusterv1.ClusterVariable{variableOf("storage", `"ceph"`)}
	if err := f.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.storages(`[
		{"storage":"ceph","node":"pve1","status":"available","shared":1,"disk":103079215104,"maxdisk":107374182400},
		{"storage":"ceph","node":"pve2","status":"available","shared":1,"disk":103079215104,"maxdisk":107374182400}
	]`)
	cond = f.look(time.Minute, StorageCondition)
	if cond == nil || cond.Reason != StorageFull || MessageHead(cond.Message) != "Storage ceph is 96% full (96 of 100 GiB)" {
		t.Fatalf("shared storage %+v", cond)
	}
	f.storages(`[{"storage":"ceph","node":"pve1","status":"available","shared":1,"disk":0,"maxdisk":107374182400}]`)
	if cond := f.look(time.Minute, StorageCondition); cond != nil {
		t.Fatalf("freed: %+v", cond)
	}
	if e := f.event(); !strings.HasPrefix(e, "Warning StorageNearlyFull Storage ceph is 96% full") {
		t.Errorf("event %q", e)
	}
	if e := f.event(); e != "Normal StorageFreed storage ceph is below 85% full" {
		t.Errorf("event %q", e)
	}
}

// A cluster being restored has its VMs stopped on purpose.
func TestVMWatchLeavesARestore(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		t.Fatal(err)
	}
	c.Annotations = map[string]string{EtcdRestoreAnnotation: "{}"}
	if err := f.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.vms(map[int64]string{101: "stopped", 102: "stopped", 103: "stopped", 104: "stopped"})
	f.look(0, VMsCondition)
	if cond := f.look(vmStateGrace, VMsCondition); cond != nil {
		t.Fatalf("condition %+v", cond)
	}
}
