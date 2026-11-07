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

// defaultCeiling is the ceiling of a cluster that sets none.
const defaultCeiling = DefaultHostOutageGraceMinutes * time.Minute

// grace sets the cluster's hostOutageGraceMinutes.
func (f *vmFixture) grace(minutes string) {
	f.t.Helper()
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(f.t.Context(), webKey, c); err != nil {
		f.t.Fatal(err)
	}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, variableOf(HostOutageGraceVariable, minutes))
	if err := f.kube.Update(f.t.Context(), c); err != nil {
		f.t.Fatal(err)
	}
}

func TestHoldCeilingIsTheClustersOwn(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.grace("45")
	f.nodes([]string{"pve2"})
	f.look(0, RemediationHeldCondition)
	wantHeld(t, f.heldMachines(), heldSecond)
	if got := f.machine(heldSecond).Annotations[HoldUntilAnnotation]; got != f.now.Add(45*time.Minute).Format(time.RFC3339) {
		t.Errorf("hold until %q", got)
	}
	if cond := f.look(44*time.Minute, RemediationHeldCondition); cond == nil {
		t.Fatal("released before the cluster's ceiling, though the default has passed")
	}
	if cond := f.look(time.Minute, RemediationHeldCondition); cond != nil {
		t.Fatalf("held past the cluster's ceiling: %+v", cond)
	}
}

func TestHoldWithoutACeilingWaitsForTheHost(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.grace("0")
	f.nodes([]string{"pve2"})
	cond := f.look(0, RemediationHeldCondition)
	if cond == nil || !strings.HasSuffix(cond.Message, "replacement waits for the host") {
		t.Fatalf("condition %+v", cond)
	}
	if _, ok := f.machine(heldSecond).Annotations[HoldUntilAnnotation]; ok {
		t.Error("an end is recorded for a hold without one")
	}
	if cond := f.look(7*24*time.Hour, RemediationHeldCondition); cond == nil {
		t.Fatal("released after a week with the host away")
	}
	wantHeld(t, f.heldMachines(), heldSecond)
	f.nodes(nil)
	f.nodeReady(heldSecond)
	if cond := f.look(time.Minute, RemediationHeldCondition); cond != nil {
		t.Fatalf("host back: %+v", cond)
	}
}

func TestValidatorHoldsTheHostOutageGrace(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	for value, ok := range map[string]bool{"0": true, "5": true, "30": true, "10080": true, "1": false, "4": false, "10081": false, "-5": false, `"soon"`: false} {
		_, err := v.ValidateCreate(t.Context(), cluster(variableOf(HostOutageGraceVariable, value)))
		refused := err != nil && strings.Contains(err.Error(), "variables["+HostOutageGraceVariable+"]")
		if refused == ok {
			t.Errorf("%s: refused %v: %v", value, refused, err)
		}
	}
}

func TestAMalformedGraceFailsTheWatch(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.grace(`"soon"`)
	look, err := f.watch.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(t.Context(), webKey, c); err != nil {
		t.Fatal(err)
	}
	if err := f.watch.watch(t.Context(), c, look); err == nil {
		t.Error("no error for a ceiling that is not a number")
	}
}

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
	until := f.now.Add(defaultCeiling).Format("15:04 MST")
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
	f.nodeNotReady()
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
	if cond := f.look(defaultCeiling-time.Second, RemediationHeldCondition); cond == nil {
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
