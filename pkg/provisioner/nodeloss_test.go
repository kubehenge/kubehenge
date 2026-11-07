package provisioner

import (
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deleting has Cluster API start deleting a machine and capmox hold its ProxmoxMachine on the
// finalizer, as it does while the VM cannot be destroyed.
func (f *vmFixture) deleting() {
	f.t.Helper()
	ctx := f.t.Context()
	name := heldSecond
	pm := f.proxmoxMachine(name)
	pm.SetFinalizers([]string{capmoxFinalizer})
	if err := f.kube.Update(ctx, pm); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, f.machine(name)); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, f.proxmoxMachine(name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *vmFixture) proxmoxMachine(name string) *unstructured.Unstructured {
	f.t.Helper()
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	if err := f.kube.Get(f.t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, pm); err != nil {
		f.t.Fatal(err)
	}
	return pm
}

// waitingOnCapmox tells whether the machine's ProxmoxMachine is still there on capmox's finalizer.
func (f *vmFixture) waitingOnCapmox() bool {
	f.t.Helper()
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	err := f.kube.Get(f.t.Context(), client.ObjectKey{Namespace: "team-a", Name: heldSecond}, pm)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return slices.Contains(pm.GetFinalizers(), capmoxFinalizer)
}

func TestAMachineOnAGoneNodeFinishesDeleting(t *testing.T) {
	t.Parallel()
	signals := map[string]func(*vmFixture){
		"the HA manager has the node gone": func(f *vmFixture) {
			f.haStates(map[string]string{"pve1": "online", "pve2": "gone", "pve3": "online", "pve4": "online"})
		},
		"the node left the cluster's members": func(f *vmFixture) { f.members("pve1", "pve3", "pve4") },
	}
	for name, signal := range signals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newVMFixture(t)
			f.nodes([]string{"pve2"})
			f.deleting()
			signal(f)
			f.look(0, RemediationHeldCondition)
			if f.waitingOnCapmox() {
				t.Fatal("the machine of a node that is gone for good still waits for capmox")
			}
			event := f.event()
			if !strings.Contains(event, MachineReleasedReason) || !strings.Contains(event, "pve2") || !strings.Contains(event, "VM 102") {
				t.Errorf("event %q names neither the node nor the VM", event)
			}
		})
	}
}

func proxmoxClusterObject() *unstructured.Unstructured {
	pc := &unstructured.Unstructured{}
	pc.SetGroupVersionKind(proxmoxMachineGVK.GroupVersion().WithKind("ProxmoxCluster"))
	return pc
}

// addLocations gives web a ProxmoxCluster that records where web-cp-1 (the deleting machine) and web-cp-0 run.
func (f *vmFixture) addLocations() {
	f.t.Helper()
	pc := proxmoxClusterObject()
	pc.SetNamespace("team-a")
	pc.SetName("web")
	pc.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web"})
	loc := func(name, node string) any {
		return map[string]any{"machine": map[string]any{"name": name}, "node": node}
	}
	pc.Object["status"] = map[string]any{"nodeLocations": map[string]any{
		"controlPlane": []any{loc("web-cp-0", "pve1"), loc(heldSecond, "pve2")},
		"workers":      []any{loc("web-md-0", "pve1")},
	}}
	if err := f.kube.Create(f.t.Context(), pc); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.Status().Update(f.t.Context(), pc); err != nil {
		f.t.Fatal(err)
	}
}

// located lists the machines the ProxmoxCluster records.
func (f *vmFixture) located() []string {
	f.t.Helper()
	pc := proxmoxClusterObject()
	if err := f.kube.Get(f.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, pc); err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for _, role := range []string{"controlPlane", "workers"} {
		locations, _, _ := unstructured.NestedSlice(pc.Object, "status", "nodeLocations", role)
		for _, l := range locations {
			entry, _ := l.(map[string]any)
			name, _, _ := unstructured.NestedString(entry, "machine", "name")
			names = append(names, name)
		}
	}
	return names
}

// stallAge moves the clock to age after the machine began deleting.
func (f *vmFixture) stallAge(age time.Duration) {
	f.t.Helper()
	f.now = f.machine(heldSecond).DeletionTimestamp.Add(age)
}

func (f *vmFixture) ask(value string) {
	f.t.Helper()
	m := f.machine(heldSecond)
	m.Annotations = map[string]string{ForgetAnnotation: value}
	if err := f.kube.Update(f.t.Context(), m); err != nil {
		f.t.Fatal(err)
	}
}

func (f *vmFixture) forgetRefusal() string {
	return f.machine(heldSecond).Annotations[ForgetRefusedAnnotation]
}

func TestAMachineStuckDeletingSaysWhy(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(*vmFixture)
		want  string
	}{
		"node offline":    {func(f *vmFixture) { f.nodes([]string{"pve2"}); f.members("pve1", "pve2", "pve3", "pve4") }, "pve2 is offline"},
		"node maintained": {func(f *vmFixture) { f.nodes(nil, "pve2") }, "in maintenance"},
		"VM not listed":   {func(f *vmFixture) { f.vms(map[int64]string{101: "running", 103: "running", 104: "running"}) }, "VM 102 is not listed"},
		"node answers":    {func(f *vmFixture) { f.nodes(nil) }, "though node pve2 answers"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newVMFixture(t)
			f.deleting()
			tc.setup(f)
			f.stallAge(DeletionStallAfter - time.Minute)
			f.look(0, RemediationHeldCondition)
			if cond := meta.FindStatusCondition(f.machine(heldSecond).Status.Conditions, MachineDeletionStalledCondition); cond != nil {
				t.Fatalf("stalled after only %s: %+v", DeletionStallAfter-time.Minute, cond)
			}
			f.stallAge(DeletionStallAfter)
			f.look(0, RemediationHeldCondition)
			cond := meta.FindStatusCondition(f.machine(heldSecond).Status.Conditions, MachineDeletionStalledCondition)
			if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, tc.want) {
				t.Fatalf("condition %+v, want a message with %q", cond, tc.want)
			}
		})
	}
}

func TestForgettingAMachineOfAnOfflineNodeReleasesItAndItsLocation(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodes([]string{"pve2"})
	f.members("pve1", "pve2", "pve3", "pve4")
	f.addLocations()
	f.deleting()
	f.ask(heldSecond)
	f.stallAge(DeletionStallAfter)
	f.look(0, RemediationHeldCondition)
	if f.waitingOnCapmox() {
		t.Fatal("the forgotten machine still waits for capmox")
	}
	if got := f.located(); slices.Contains(got, heldSecond) || len(got) != 2 {
		t.Fatalf("node locations %v still hold the forgotten machine, or lost another", got)
	}
}

func TestForgettingIsRefusedWhereReleasingCouldHarm(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(*vmFixture)
		age   time.Duration
		value string
		want  string
	}{
		"too soon":      {func(f *vmFixture) { f.nodes([]string{"pve2"}) }, time.Minute, heldSecond, "only"},
		"wrong name":    {func(f *vmFixture) { f.nodes([]string{"pve2"}) }, DeletionStallAfter, "web-cp-0", "not the machine"},
		"VM still runs": {func(f *vmFixture) { f.nodes(nil) }, DeletionStallAfter, heldSecond, "Stop it first"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newVMFixture(t)
			f.deleting()
			tc.setup(f)
			f.ask(tc.value)
			f.stallAge(tc.age)
			f.look(0, RemediationHeldCondition)
			if !f.waitingOnCapmox() {
				t.Fatal("a refused request released the machine")
			}
			if got := f.forgetRefusal(); !strings.Contains(got, tc.want) {
				t.Fatalf("refusal %q, want it to say %q", got, tc.want)
			}
			if _, still := f.machine(heldSecond).Annotations[ForgetAnnotation]; still {
				t.Error("a refused request stays on the machine, so it would be answered again")
			}
		})
	}
}

func TestForgettingAMachineWhoseVMIsStoppedOnAnOnlineNodeIsAllowed(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.vms(map[int64]string{101: "running", 102: "stopped", 103: "running", 104: "running"})
	f.deleting()
	f.ask(heldSecond)
	f.stallAge(DeletionStallAfter)
	f.look(0, RemediationHeldCondition)
	if f.waitingOnCapmox() {
		t.Fatal("a stopped VM on an online node blocked the request")
	}
}

func TestAMachineOnAnOfflineNodeKeepsWaiting(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodes([]string{"pve2"})
	f.members("pve1", "pve2", "pve3", "pve4")
	f.deleting()
	for _, after := range []time.Duration{0, time.Hour, 7 * 24 * time.Hour} {
		f.look(after, RemediationHeldCondition)
		if !f.waitingOnCapmox() {
			t.Fatalf("after %s the VM of a node that is only offline was let go", after)
		}
	}
}

func TestAMachineWhoseNodeReturnsMidDeleteIsLeftToCapmox(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodes([]string{"pve2"})
	f.members("pve1", "pve2", "pve3", "pve4")
	f.deleting()
	f.look(0, RemediationHeldCondition)
	f.nodes(nil)
	f.look(time.Minute, RemediationHeldCondition)
	if !f.waitingOnCapmox() {
		t.Fatal("the VM of a node that came back was let go, though capmox can now destroy it")
	}
	if event := f.event(); event != "" {
		t.Errorf("event %q", event)
	}
}

func TestOnlyMachinesBeingDeletedAreLetGo(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodes([]string{"pve2"})
	pm := f.proxmoxMachine(heldSecond)
	pm.SetFinalizers([]string{capmoxFinalizer})
	if err := f.kube.Update(t.Context(), pm); err != nil {
		t.Fatal(err)
	}
	f.haStates(map[string]string{"pve1": "online", "pve2": "gone", "pve3": "online", "pve4": "online"})
	f.look(0, RemediationHeldCondition)
	if !f.waitingOnCapmox() {
		t.Error("the finalizer of a machine that is not being deleted was removed")
	}
}

func TestADeletingClusterLetsGoOfMachinesOnAGoneNode(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodes([]string{"pve2"})
	f.deleting()
	f.haStates(map[string]string{"pve1": "online", "pve2": "gone", "pve3": "online", "pve4": "online"})
	look, err := f.watch.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(t.Context(), webKey, c); err != nil {
		t.Fatal(err)
	}
	c.DeletionTimestamp = &metav1.Time{Time: f.now}
	if err := f.watch.watch(t.Context(), c, look); err != nil {
		t.Fatal(err)
	}
	if f.waitingOnCapmox() {
		t.Fatal("a cluster being deleted still waits on a node that is gone")
	}
}
