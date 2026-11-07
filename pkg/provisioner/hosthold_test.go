package provisioner

import (
	"fmt"
	"strings"
	"testing"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	heldFirst  = "web-cp-0"
	heldSecond = "web-cp-1"
	heldThird  = "web-cp-2"
)

var controlPlanes = []string{heldFirst, heldSecond, heldThird}

// heldMachines are the control planes Kubehenge keeps Cluster API from replacing.
func (f *vmFixture) heldMachines() []string {
	f.t.Helper()
	var held []string
	for _, name := range controlPlanes {
		if f.held(name) {
			held = append(held, name)
		}
	}
	return held
}

func (f *vmFixture) allReady() {
	f.t.Helper()
	for _, name := range controlPlanes {
		f.nodeReady(name)
	}
}

func wantHeld(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("held machines %v, want %v", got, want)
	}
}

func TestHoldKeepsMachinesOfAnOfflineNodeFromReplacement(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.allReady()
	if cond := f.look(0, RemediationHeldCondition); cond != nil {
		t.Fatalf("all nodes online: %+v", cond)
	}
	wantHeld(t, f.heldMachines())
	// pve4 is offline too, and none of web's VMs runs there.
	f.nodes([]string{"pve2", "pve4"})
	cond := f.look(time.Minute, RemediationHeldCondition)
	if cond == nil || cond.Reason != HeldNodeOffline {
		t.Fatalf("condition %+v", cond)
	}
	wantHeld(t, f.heldMachines(), heldSecond)
	if why := f.machine(heldSecond).Annotations[HoldAnnotation]; why != "Proxmox VE node pve2 is offline" {
		t.Errorf("the record says %q", why)
	}
	until := f.now.Add(HoldCeiling).Format("15:04 MST")
	if !strings.Contains(cond.Message, heldSecond+": Proxmox VE node pve2 is offline, replacement waits until "+until) {
		t.Errorf("message %q", cond.Message)
	}
	if got := f.machine(heldSecond).Annotations[HoldSinceAnnotation]; got != f.now.Format(time.RFC3339) {
		t.Errorf("since %q", got)
	}
}

func TestHoldKeepsMachinesOfANodeInMaintenanceFromReplacement(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.allReady()
	f.nodes(nil, "pve3", "pve4")
	cond := f.look(0, RemediationHeldCondition)
	if cond == nil || cond.Reason != HeldNodeMaintenance || !strings.Contains(cond.Message, "pve3 is in maintenance") {
		t.Fatalf("condition %+v", cond)
	}
	wantHeld(t, f.heldMachines(), heldThird)
	f.nodes([]string{"pve1"}, "pve3")
	if cond := f.look(0, RemediationHeldCondition); cond == nil || cond.Reason != HeldNodeOffline {
		t.Errorf("offline outranks maintenance: %+v", cond)
	}
	wantHeld(t, f.heldMachines(), heldFirst, heldThird)
}

func TestHoldLiftsWhenTheNodeIsBackAndTheMachinesNodeReady(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodeNotReady(heldSecond)
	f.nodes([]string{"pve2"})
	f.look(0, RemediationHeldCondition)
	wantHeld(t, f.heldMachines(), heldSecond)
	f.nodes(nil)
	cond := f.look(time.Minute, RemediationHeldCondition)
	if cond == nil || cond.Reason != HeldNodeRecovering {
		t.Fatalf("the node is back but its Kubernetes node is not Ready: %+v", cond)
	}
	wantHeld(t, f.heldMachines(), heldSecond)
	f.nodeReady(heldSecond)
	if cond := f.look(time.Minute, RemediationHeldCondition); cond != nil {
		t.Fatalf("back and Ready: %+v", cond)
	}
	wantHeld(t, f.heldMachines())
	if _, left := f.machine(heldSecond).Annotations[HoldSinceAnnotation]; left {
		t.Error("the hold's start is left on the machine")
	}
	if e := f.event(); !strings.Contains(e, RemediationHeldCondition) {
		t.Errorf("event %q", e)
	}
}

func TestHoldLiftsAtTheCeilingAndDoesNotReturnForTheSameOutage(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodes([]string{"pve2"})
	f.look(0, RemediationHeldCondition)
	wantHeld(t, f.heldMachines(), heldSecond)
	if cond := f.look(HoldCeiling-time.Second, RemediationHeldCondition); cond == nil {
		t.Fatal("released before the ceiling")
	}
	wantHeld(t, f.heldMachines(), heldSecond)
	if cond := f.look(time.Second, RemediationHeldCondition); cond != nil {
		t.Fatalf("at the ceiling: %+v", cond)
	}
	wantHeld(t, f.heldMachines())
	// The node is still gone: the machine stays free to be replaced.
	if cond := f.look(time.Minute, RemediationHeldCondition); cond != nil {
		t.Fatalf("held again for the same outage: %+v", cond)
	}
	wantHeld(t, f.heldMachines())
	// A later outage is a new one.
	f.nodes(nil)
	f.nodeReady(heldSecond)
	f.look(time.Minute, RemediationHeldCondition)
	f.nodes([]string{"pve2"})
	if cond := f.look(time.Minute, RemediationHeldCondition); cond == nil {
		t.Fatal("a second outage is not held")
	}
	wantHeld(t, f.heldMachines(), heldSecond)
}

func TestHoldLeavesAnAdministratorsSkipAlone(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	m := f.machine(heldSecond)
	m.Annotations = map[string]string{clusterv1.MachineSkipRemediationAnnotation: ""}
	if err := f.kube.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	f.nodes([]string{"pve2"})
	if cond := f.look(0, RemediationHeldCondition); cond != nil {
		t.Errorf("condition %+v", cond)
	}
	f.nodes(nil)
	f.nodeReady(heldSecond)
	f.look(time.Minute, RemediationHeldCondition)
	a := f.machine(heldSecond).Annotations
	if _, ok := a[clusterv1.MachineSkipRemediationAnnotation]; !ok || len(a) != 1 {
		t.Errorf("annotations %v", a)
	}
}

const sharedStorage = `{"storage":"local-lvm","node":%q,"status":%q,"shared":1,"disk":1,"maxdisk":100}`

func (f *vmFixture) sharedStorage(statuses map[string]string) {
	f.t.Helper()
	entries := make([]string, 0, len(statuses))
	for _, n := range []string{"pve1", "pve2", "pve3"} {
		entries = append(entries, fmt.Sprintf(sharedStorage, n, statuses[n]))
	}
	f.storages("[" + strings.Join(entries, ",") + "]")
}

func TestStorageUnavailableOnOneNodeOfASharedStorage(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.allReady()
	f.sharedStorage(map[string]string{"pve1": "available", "pve2": "unknown", "pve3": "available"})
	cond := f.look(0, StorageUnavailableCondition)
	if cond == nil || cond.Reason != StorageUnavailableReason || MessageHead(cond.Message) != "Storage local-lvm is unavailable on pve2" {
		t.Fatalf("condition %+v", cond)
	}
	held := f.look(0, RemediationHeldCondition)
	if held == nil || held.Reason != HeldStorageUnavailable {
		t.Fatalf("hold %+v", held)
	}
	wantHeld(t, f.heldMachines(), heldSecond)
	if why := f.machine(heldSecond).Annotations[HoldAnnotation]; why != "Storage local-lvm is unavailable on pve2" {
		t.Errorf("the record says %q", why)
	}
	f.sharedStorage(map[string]string{"pve1": "available", "pve2": "available", "pve3": "available"})
	if cond := f.look(time.Minute, StorageUnavailableCondition); cond != nil {
		t.Errorf("storage back: %+v", cond)
	}
	wantHeld(t, f.heldMachines())
}

func TestStorageUnavailableOnEveryNode(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.allReady()
	f.sharedStorage(map[string]string{"pve1": "unknown", "pve2": "unknown", "pve3": "unknown"})
	cond := f.look(0, StorageUnavailableCondition)
	if cond == nil || MessageHead(cond.Message) != "Storage local-lvm is unavailable on pve1, pve2, pve3" {
		t.Fatalf("condition %+v", cond)
	}
	wantHeld(t, f.heldMachines(), heldFirst, heldSecond, heldThird)
}

func TestStorageOfAnOfflineNodeIsTheNodesToSay(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.allReady()
	f.nodes([]string{"pve2"})
	f.sharedStorage(map[string]string{"pve1": "available", "pve2": "unknown", "pve3": "available"})
	if cond := f.look(0, StorageUnavailableCondition); cond != nil {
		t.Errorf("an offline node's storage is listed unknown: %+v", cond)
	}
	if cond := f.look(0, RemediationHeldCondition); cond == nil || cond.Reason != HeldNodeOffline {
		t.Errorf("hold %+v", cond)
	}
}

func TestStorageOfNodesWithoutTheClustersVMsIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.allReady()
	f.storages(`[{"storage":"local-lvm","node":"pve1","status":"available"},{"storage":"local-lvm","node":"pve4","status":"unknown"},{"storage":"other","node":"pve2","status":"unknown"}]`)
	if cond := f.look(0, StorageUnavailableCondition); cond != nil {
		t.Errorf("condition %+v", cond)
	}
	wantHeld(t, f.heldMachines())
}
