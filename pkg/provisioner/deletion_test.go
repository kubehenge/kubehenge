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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimev1 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	deletedAt  = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	controlGVK = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Kind: "RKE2ControlPlane"}
	webKey     = client.ObjectKey{Namespace: "team-a", Name: "web"}

	proxmoxClusterGVK = proxmoxMachineGVK.GroupVersion().WithKind("ProxmoxCluster")
)

// flakyCloud cannot remove a cluster's Proxmox VE objects for its first failures calls.
type flakyCloud struct {
	fakeCloud

	failures int
}

func (f *flakyCloud) Remove(ctx context.Context, names Names, held Credentials) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("node pve2 is offline")
	}
	return f.fakeCloud.Remove(ctx, names, held)
}

// failingCloud cannot remove a cluster's Proxmox VE objects, as when a node holding its volumes is
// down.
type failingCloud struct{ fakeCloud }

func (*failingCloud) Remove(context.Context, Names, Credentials) error {
	return errors.New("node pve2 is offline")
}

type deletionFixture struct {
	t      *testing.T
	kube   client.Client
	r      *Reconciler
	events *events.FakeRecorder
	now    time.Time
}

// newDeletionFixture is a cluster web being deleted since deletedAt, whose worker machine waits on
// its node's drain, beside its ProxmoxMachine with VM 101 and a control plane not yet deleted.
func newDeletionFixture(t *testing.T, cloud Cloud, edit func(*clusterv1.Cluster)) *deletionFixture {
	t.Helper()
	return newDeletionFixtureWith(t, cloud, edit, nil)
}

// newDeletionFixtureWith is newDeletionFixture with editMachine applied to the ProxmoxMachine.
func newDeletionFixtureWith(t *testing.T, cloud Cloud, edit func(*clusterv1.Cluster), editMachine func(*unstructured.Unstructured)) *deletionFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{clusterv1.GroupVersion, proxmoxMachineGVK.GroupVersion(), controlGVK.GroupVersion()})
	mapper.Add(clusterv1.GroupVersion.WithKind("Machine"), meta.RESTScopeNamespace)
	mapper.Add(clusterv1.GroupVersion.WithKind("Cluster"), meta.RESTScopeNamespace)
	for _, gvk := range []schema.GroupVersionKind{proxmoxMachineGVK, proxmoxClusterGVK, controlGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		list := gvk
		list.Kind += "List"
		scheme.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	c := storageCluster()
	c.Finalizers = []string{Finalizer, "cluster.cluster.x-k8s.io"}
	c.DeletionTimestamp = &metav1.Time{Time: deletedAt}
	if edit != nil {
		edit(c)
	}
	labels := map[string]string{clusterv1.ClusterNameLabel: "web"}
	worker := &clusterv1.Machine{
		Name: "web-md-0-a", Namespace: "team-a", Labels: labels,
		Finalizers: []string{clusterv1.MachineFinalizer}, DeletionTimestamp: &metav1.Time{Time: deletedAt.Add(time.Minute)},
		Status: clusterv1.MachineStatus{Conditions: []metav1.Condition{{
			Type: clusterv1.DeletingCondition, Status: metav1.ConditionTrue, Reason: "DrainingNode",
			Message: "Drain not completed yet (started at 10:01):\n* Pod web/db-0: cannot evict pod",
		}}},
	}
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace("team-a")
	pm.SetName("web-md-0-a")
	pm.SetLabels(labels)
	pm.SetFinalizers([]string{"proxmoxmachine.infrastructure.cluster.x-k8s.io"})
	_ = unstructured.SetNestedField(pm.Object, int64(101), "spec", "virtualMachineID")
	_ = unstructured.SetNestedField(pm.Object, "pve1", "status", "proxmoxNode")
	if editMachine != nil {
		editMachine(pm)
	}
	control := &unstructured.Unstructured{}
	control.SetGroupVersionKind(controlGVK)
	control.SetNamespace("team-a")
	control.SetName("web-cp")
	control.SetLabels(labels)
	control.SetFinalizers([]string{"rke2.controlplane.cluster.x-k8s.io"})
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(c, worker, pm, control).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	f := &deletionFixture{t: t, kube: kube, events: events.NewFakeRecorder(50), now: deletedAt.Add(5 * time.Minute)}
	f.r = &Reconciler{
		Client: kube, Cloud: cloud, WorkloadPool: "kubehenge", ClassNamespace: "kubehenge-system",
		Events: f.events, Now: func() time.Time { return f.now },
	}
	return f
}

func (f *deletionFixture) reconcile() {
	f.t.Helper()
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: webKey}); err != nil {
		f.t.Fatal(err)
	}
}

// cluster is web as it is now, nil once it is gone.
func (f *deletionFixture) cluster() *clusterv1.Cluster {
	f.t.Helper()
	c := &clusterv1.Cluster{}
	err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *deletionFixture) drained() []string {
	var got []string
	for {
		select {
		case e := <-f.events.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func (f *deletionFixture) gone(gvk schema.GroupVersionKind, name string) bool {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	return apierrors.IsNotFound(f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, o))
}

// A deletion says what it waits on, on the Cluster, and once it has waited longer than it should, a
// condition and an event say so, with the way out.
func TestDeletionSaysWhatItWaitsOn(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &fakeCloud{}, nil)
	f.reconcile()
	st := DeletionStatusOf(f.cluster())
	// The ProxmoxMachine and the control plane, not being deleted yet, wait on nothing.
	if len(st.Waiting) != 2 {
		t.Fatalf("waiting %+v", st.Waiting)
	}
	if w := st.Waiting[0]; w.Kind != "Hook" || w.Name != "BeforeClusterDelete" {
		t.Errorf("first wait %+v, want the hook Cluster API waits for", w)
	}
	machine := st.Waiting[1]
	if machine.Kind != "Machine" || machine.Name != "web-md-0-a" || !strings.HasPrefix(machine.Reason, "Drain not completed yet (started at 10:01): * Pod web/db-0") {
		t.Errorf("machine wait %+v", machine)
	}
	if want := []string{"Rancher registration", "VM 101 on pve1", "Pool kubehenge/team-a.web"}; !slices.Equal(st.MayLeave, want) {
		t.Errorf("may leave %v, want %v", st.MayLeave, want)
	}
	if cond := meta.FindStatusCondition(f.cluster().Status.Conditions, DeletionStalledCondition); cond != nil {
		t.Errorf("stalled after five minutes: %+v", cond)
	}

	f.now = deletedAt.Add(25 * time.Minute)
	f.reconcile()
	cond := meta.FindStatusCondition(f.cluster().Status.Conditions, DeletionStalledCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition %+v", cond)
	}
	for _, want := range []string{"waited 25m0s on Hook BeforeClusterDelete (leaving Rancher), Machine web-md-0-a (Drain", "kubehenge.dev/abandon-deletion=web"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message has no %q: %s", want, cond.Message)
		}
	}
	if head := DeletionStallHead(cond.Message); strings.Contains(head, "kubectl") || !strings.HasSuffix(head, ".") {
		t.Errorf("head %q", head)
	}
	if got := f.drained(); len(got) != 1 || !strings.HasPrefix(got[0], "Warning "+DeletionStalledCondition) {
		t.Errorf("events %q", got)
	}
	f.reconcile()
	if got := f.drained(); len(got) != 0 {
		t.Errorf("events %q on a second look", got)
	}
}

// A VM that capmox cannot destroy shows what it says on the ProxmoxMachine, whole: the flag set on
// the VM, or a VM of another name under its ID, and what to do about it.
func TestDeletionQuotesWhyAVMIsNotDestroyed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, message string }{
		{"protected", "VM 101 has its protection flag set, which stops Proxmox VE destroying it. Clear the flag with: qm set 101 --protection 0"},
		{"another name", `VM 101 is named "db-old", not "web-md-0-a", and the machine has no provider ID, so it is not destroyed. If it is the machine's VM, rename it to "web-md-0-a".`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDeletionFixtureWith(t, &fakeCloud{}, nil, func(pm *unstructured.Unstructured) {
				pm.SetDeletionTimestamp(&metav1.Time{Time: deletedAt.Add(time.Minute)})
				// Cluster API's summary of the provisioned condition, as capmox's Ready condition carries it.
				_ = unstructured.SetNestedSlice(pm.Object, []any{map[string]any{
					"type": clusterv1.ReadyCondition, "status": "False", "reason": "NotReady",
					"message": "* VirtualMachineProvisioned: " + tc.message,
				}}, "status", "conditions")
			})
			f.reconcile()
			var got string
			for _, w := range DeletionStatusOf(f.cluster()).Waiting {
				if w.Kind == "ProxmoxMachine" {
					got = w.Reason
				}
			}
			if want := "* VirtualMachineProvisioned: " + tc.message; got != want {
				t.Errorf("reason %q, want %q", got, want)
			}
		})
	}
}

// The machines gone, a failing removal of the cluster's Proxmox VE objects is what the deletion
// waits on.
func TestDeletionSaysKubehengesOwnWait(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &failingCloud{}, func(c *clusterv1.Cluster) {
		c.Annotations = map[string]string{runtimev1.OkToDeleteAnnotation: ""}
	})
	for _, gvk := range []schema.GroupVersionKind{clusterv1.GroupVersion.WithKind("Machine"), proxmoxMachineGVK} {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(gvk)
		o.SetName("web-md-0-a")
		o.SetNamespace("team-a")
		if err := f.kube.Patch(context.Background(), o, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: webKey})
	if err == nil {
		t.Fatal("no error from a failed removal")
	}
	st := DeletionStatusOf(f.cluster())
	if len(st.Waiting) != 1 || st.Waiting[0].Kind != "Cluster" || !strings.Contains(st.Waiting[0].Reason, "node pve2 is offline") {
		t.Errorf("waiting %+v", st.Waiting)
	}
}

// An abandoned deletion deletes what is left and lets machines go without a drain. What has not
// finished by the grace loses its finalizers, and the Cluster its own last, each leftover named.
func TestAbandonedDeletion(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &fakeCloud{}, func(c *clusterv1.Cluster) {
		c.Annotations = map[string]string{AbandonAnnotation: "web"}
	})
	f.reconcile()
	c := f.cluster()
	st := DeletionStatusOf(c)
	if st.Abandoned == nil || !st.Abandoned.Time.Equal(f.now) {
		t.Fatalf("abandoned %v", st.Abandoned)
	}
	if _, ok := c.Annotations[runtimev1.OkToDeleteAnnotation]; !ok {
		t.Error("Cluster API still waits for BeforeClusterDelete")
	}
	machine := &clusterv1.Machine{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-md-0-a"}, machine); err != nil {
		t.Fatal(err)
	}
	if _, ok := machine.Annotations[clusterv1.ExcludeNodeDrainingAnnotation]; !ok {
		t.Error("the machine still drains its node")
	}
	control := &unstructured.Unstructured{}
	control.SetGroupVersionKind(controlGVK)
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-cp"}, control); err != nil || control.GetDeletionTimestamp() == nil {
		t.Errorf("control plane not deleted: %v", err)
	}
	if !slices.Equal(st.LeftBehind, []string{"Rancher registration"}) {
		t.Errorf("left behind %v before the grace", st.LeftBehind)
	}

	f.now = f.now.Add(AbandonGrace)
	f.reconcile()
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{{clusterv1.GroupVersion.WithKind("Machine"), "web-md-0-a"}, {proxmoxMachineGVK, "web-md-0-a"}, {controlGVK, "web-cp"}} {
		if !f.gone(o.gvk, o.name) {
			t.Errorf("%s %s is still there", o.gvk.Kind, o.name)
		}
	}
	if st = DeletionStatusOf(f.cluster()); !slices.Contains(st.LeftBehind, "VM 101 on pve1") {
		t.Errorf("left behind %v", st.LeftBehind)
	}

	f.reconcile()
	if f.cluster() != nil {
		t.Fatal("the cluster is still there")
	}
	got := strings.Join(f.drained(), "\n")
	for _, want := range []string{
		"Warning AbandoningDeletion",
		"Warning LeftBehind left behind by the abandoned deletion: VM 101 on pve1",
		"Warning DeletionAbandoned deletion abandoned; left behind: Rancher registration, VM 101 on pve1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no event %q in:\n%s", want, got)
		}
	}
}

// What cannot be removed of an abandoned cluster's Proxmox VE objects is left behind too.
func TestAbandonedDeletionLeavesThePool(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &failingCloud{}, func(c *clusterv1.Cluster) {
		c.Annotations = map[string]string{AbandonAnnotation: "web", runtimev1.OkToDeleteAnnotation: ""}
	})
	f.reconcile()
	f.now = f.now.Add(AbandonGrace)
	f.reconcile()
	f.reconcile()
	if f.cluster() != nil {
		t.Fatal("the cluster is still there")
	}
	got := strings.Join(f.drained(), "\n")
	if !strings.Contains(got, "left behind: VM 101 on pve1, Pool kubehenge/team-a.web and user") {
		t.Errorf("events:\n%s", got)
	}
}

// drop removes an object as its controller would, finalizers and all.
func (f *deletionFixture) drop(gvk schema.GroupVersionKind, name string) {
	f.t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetNamespace("team-a")
	o.SetName(name)
	ctx := context.Background()
	err := f.kube.Patch(ctx, o, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
	if client.IgnoreNotFound(err) != nil {
		f.t.Fatal(err)
	}
	err = f.kube.Delete(ctx, o)
	if client.IgnoreNotFound(err) != nil {
		f.t.Fatal(err)
	}
}

// addProxmoxCluster adds web's ProxmoxCluster, which capmox keeps until no ProxmoxMachine is left.
func (f *deletionFixture) addProxmoxCluster() {
	f.t.Helper()
	pc := &unstructured.Unstructured{}
	pc.SetGroupVersionKind(proxmoxClusterGVK)
	pc.SetNamespace("team-a")
	pc.SetName("web")
	pc.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web"})
	pc.SetFinalizers([]string{"proxmoxcluster.infrastructure.cluster.x-k8s.io"})
	if err := f.kube.Create(context.Background(), pc); err != nil {
		f.t.Fatal(err)
	}
}

// reconcileUntilGone reconciles up to n times and tells whether the cluster is gone by then.
func (f *deletionFixture) reconcileUntilGone(n int) bool {
	f.t.Helper()
	for range n {
		f.reconcile()
		if f.cluster() == nil {
			return true
		}
	}
	return false
}

func abandoning(c *clusterv1.Cluster) {
	c.Annotations = map[string]string{AbandonAnnotation: "web", runtimev1.OkToDeleteAnnotation: ""}
}

// With no ProxmoxMachine and no ProxmoxCluster left, nothing outside the management cluster can be
// left behind, so an abandoned deletion does not wait out the grace.
func TestAbandonedDeletionWithoutVMsReleasesAtOnce(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &fakeCloud{}, abandoning)
	f.drop(proxmoxMachineGVK, "web-md-0-a")
	if !f.reconcileUntilGone(5) {
		t.Fatal("the cluster is still there before the grace is up")
	}
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{{clusterv1.GroupVersion.WithKind("Machine"), "web-md-0-a"}, {controlGVK, "web-cp"}} {
		if !f.gone(o.gvk, o.name) {
			t.Errorf("%s %s is still there", o.gvk.Kind, o.name)
		}
	}
	if got := strings.Join(f.drained(), "\n"); !strings.Contains(got, "deletion abandoned; left behind: nothing") {
		t.Errorf("events:\n%s", got)
	}
}

// A ProxmoxMachine left may still have its VM, so the grace is waited out in full.
func TestAbandonedDeletionWithAVMWaitsTheGrace(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &fakeCloud{}, abandoning)
	for range 6 {
		f.reconcile()
		f.now = f.now.Add(AbandonGrace / 6)
	}
	if f.cluster() == nil || f.gone(proxmoxMachineGVK, "web-md-0-a") {
		t.Fatal("released before the grace was up")
	}
	f.now = f.now.Add(time.Second)
	if !f.reconcileUntilGone(5) {
		t.Fatal("the cluster is still there after the grace")
	}
	if got := strings.Join(f.drained(), "\n"); !strings.Contains(got, "left behind: VM 101 on pve1") {
		t.Errorf("events:\n%s", got)
	}
}

// capmox keeps a ProxmoxCluster until no ProxmoxMachine is left, then takes its finalizer off the
// credentials Secret. Releasing it early would leave that Secret's finalizer for good, so it is waited for.
func TestAbandonedDeletionWaitsForTheProxmoxCluster(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &fakeCloud{}, abandoning)
	f.drop(proxmoxMachineGVK, "web-md-0-a")
	f.addProxmoxCluster()
	for range 6 {
		f.reconcile()
	}
	if f.cluster() == nil || f.gone(proxmoxClusterGVK, "web") {
		t.Fatal("released with the ProxmoxCluster still there")
	}
	f.drop(proxmoxClusterGVK, "web")
	if !f.reconcileUntilGone(5) {
		t.Fatal("the cluster is still there after its ProxmoxCluster went")
	}
}

// When the grace runs out, everything is released and what is left behind is named.
func TestAbandonedDeletionGraceReleasesEverything(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &fakeCloud{}, abandoning)
	f.addProxmoxCluster()
	f.reconcile()
	f.now = f.now.Add(AbandonGrace)
	if !f.reconcileUntilGone(5) {
		t.Fatal("the cluster is still there after the grace")
	}
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{{proxmoxMachineGVK, "web-md-0-a"}, {proxmoxClusterGVK, "web"}, {controlGVK, "web-cp"}} {
		if !f.gone(o.gvk, o.name) {
			t.Errorf("%s %s is still there", o.gvk.Kind, o.name)
		}
	}
	if got := strings.Join(f.drained(), "\n"); !strings.Contains(got, "deletion abandoned; left behind: VM 101 on pve1") {
		t.Errorf("events:\n%s", got)
	}
}

// A removal of the Proxmox VE objects that fails is recorded as left behind once the grace is up,
// and not before.
func TestAbandonedDeletionWithoutVMsRecordsAFailedRemoval(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &failingCloud{}, abandoning)
	f.drop(proxmoxMachineGVK, "web-md-0-a")
	for range 5 {
		f.reconcile()
	}
	if f.cluster() == nil {
		t.Fatal("released before the grace, with the pool still there")
	}
	f.now = f.now.Add(AbandonGrace)
	if !f.reconcileUntilGone(5) {
		t.Fatal("the cluster is still there after the grace")
	}
	if got := strings.Join(f.drained(), "\n"); !strings.Contains(got, "left behind: Pool kubehenge/team-a.web and user") {
		t.Errorf("events:\n%s", got)
	}
}

// A removal that fails and then works within the grace leaves nothing behind, and the deletion ends
// when it works.
func TestAbandonedDeletionWithoutVMsRetriesTheRemoval(t *testing.T) {
	t.Parallel()
	f := newDeletionFixture(t, &flakyCloud{failures: 2}, abandoning)
	f.drop(proxmoxMachineGVK, "web-md-0-a")
	if !f.reconcileUntilGone(10) {
		t.Fatal("the cluster is still there before the grace is up")
	}
	if got := strings.Join(f.drained(), "\n"); !strings.Contains(got, "left behind: nothing") {
		t.Errorf("events:\n%s", got)
	}
}

func TestValidateAbandon(t *testing.T) {
	t.Parallel()
	deleting := func(value string) *clusterv1.Cluster {
		c := cluster()
		c.DeletionTimestamp = &metav1.Time{Time: deletedAt}
		c.Annotations = map[string]string{AbandonAnnotation: value}
		return c
	}
	live := cluster()
	live.Annotations = map[string]string{AbandonAnnotation: "web"}
	for _, tc := range []struct {
		name     string
		old, now *clusterv1.Cluster
		want     string
	}{
		{"a live cluster", cluster(), live, "only a cluster being deleted"},
		{"a live cluster created so", nil, live, "only a cluster being deleted"},
		{"another name", cluster(), deleting("other"), "must be the cluster's name"},
		{"its name", cluster(), deleting("web"), ""},
		{"unchanged", deleting("other"), deleting("other"), ""},
	} {
		err := validateAbandon(tc.old, tc.now)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	if Abandoned(live) || !Abandoned(deleting("web")) || Abandoned(deleting("other")) {
		t.Error("Abandoned")
	}
}
