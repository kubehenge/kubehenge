package provisioner

import (
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
