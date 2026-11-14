package provisioner

import (
	"context"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// spaceSites are Proxmox VE's storages as one look found them, with a storage local-lvm on pve1 of
// the type and free space a test sets.
type spaceSites struct {
	mu    sync.Mutex
	space []pve.Storage
}

func (s *spaceSites) Sites(context.Context) (*Sites, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &Sites{Storages: map[string]StorageSite{"local-lvm": {Nodes: []string{"pve1"}, Active: []string{"pve1"}}}, Space: s.space}, nil
}

func (s *spaceSites) set(kind string, freeGiB int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.space = []pve.Storage{{
		Storage: "local-lvm", Node: "pve1", Type: kind, Active: true, Content: []string{"images"},
		Avail: freeGiB << 30, Total: 2000 << 30,
	}}
}

// diskCluster asks for one control plane and two workers of 30 GiB each on local-lvm: 90 GiB.
func diskCluster(t *testing.T) *clusterv1.Cluster {
	t.Helper()
	web := cluster(
		clusterVariable(t, "controlPlaneMachine", map[string]int{"cores": 2, "memoryMiB": 2048, "diskGiB": 30}),
		clusterVariable(t, "workerMachine", map[string]int{"cores": 2, "memoryMiB": 2048, "diskGiB": 30}),
		clusterVariable(t, "storage", "local-lvm"),
	)
	web.Spec.Topology.ControlPlane.Replicas = ptr.To[int32](1)
	web.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: ptr.To[int32](2)}}
	return web
}

// webMachine is one of web's machines, its VM made or not.
func webMachine(name, pool string, made bool) *clusterv1.Machine {
	labels := map[string]string{clusterv1.ClusterNameLabel: "web"}
	if pool == "" {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	} else {
		labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] = pool
	}
	m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, Labels: labels}}
	m.Spec.ClusterName = "web"
	m.Status.Initialization.InfrastructureProvisioned = ptr.To(made)
	return m
}

func placedOnSpace(t *testing.T, sites *spaceSites, objs ...client.Object) (*Placer, client.Client, *clusterv1.Cluster) {
	t.Helper()
	web := diskCluster(t)
	_, c := newReconciler(t, nil, append([]client.Object{web}, objs...)...)
	nodes := &memNodes{free: map[string]int64{"pve1": 64_000}}
	return &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes, Sites: sites}, c, web
}

func spaceCondition(t *testing.T, c client.Client, web *clusterv1.Cluster) *metav1.Condition {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(got.Status.Conditions, StorageLacksSpaceCondition)
}

const shortOfSpace = "Storage local-lvm (on pve1) reserves each disk's whole size and has 10 GiB free. The machines still to be made need 90 GiB"

// A cluster made with kubectl or GitOps on thick storage without room for its disks says so, with
// the storage, its node, the room needed and the room free.
func TestPlacerSaysThickStorageLacksRoomForTheDisks(t *testing.T) {
	t.Parallel()
	sites := &spaceSites{}
	sites.set("lvm", 10)
	p, c, web := placedOnSpace(t, sites)
	p.placeAll(t.Context())
	if cond := spaceCondition(t, c, web); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "NoSpace" || cond.Message != shortOfSpace {
		t.Errorf("%s = %+v, want %q", StorageLacksSpaceCondition, cond, shortOfSpace)
	}
}

// Thin storage takes a disk's space only as it is written, so it never holds the machines back.
func TestPlacerNeverSaysThinStorageLacksRoom(t *testing.T) {
	t.Parallel()
	sites := &spaceSites{}
	sites.set("lvmthin", 10)
	p, c, web := placedOnSpace(t, sites)
	p.placeAll(t.Context())
	if cond := spaceCondition(t, c, web); cond != nil {
		t.Errorf("%s = %+v on thin storage", StorageLacksSpaceCondition, cond)
	}
}

// A running cluster's disks hold their space already: the free space left is not short for them.
// Only a machine whose VM is not made yet still needs its disk.
func TestPlacerCountsOnlyTheDisksStillToBeMade(t *testing.T) {
	t.Parallel()
	sites := &spaceSites{}
	sites.set("lvm", 10)
	p, c, web := placedOnSpace(t, sites,
		webMachine("web-cp-a", "", true), webMachine("web-md-a", "md-0", true), webMachine("web-md-b", "md-0", true))
	p.placeAll(t.Context())
	if cond := spaceCondition(t, c, web); cond != nil {
		t.Errorf("%s = %+v for a running cluster", StorageLacksSpaceCondition, cond)
	}

	// A replacement machine whose VM waits: 30 GiB still to be made, and 10 GiB free.
	if err := c.Create(t.Context(), webMachine("web-md-c", "md-0", false)); err != nil {
		t.Fatal(err)
	}
	p.placeAll(t.Context())
	want := "Storage local-lvm (on pve1) reserves each disk's whole size and has 10 GiB free. The machines still to be made need 30 GiB"
	if cond := spaceCondition(t, c, web); cond == nil || cond.Message != want {
		t.Errorf("%s = %+v, want %q", StorageLacksSpaceCondition, cond, want)
	}
}

// Once the storage has room for the disks, the condition goes.
func TestPlacerClearsTheLackOfSpaceOnceTheDisksFit(t *testing.T) {
	t.Parallel()
	sites := &spaceSites{}
	sites.set("lvm", 10)
	p, c, web := placedOnSpace(t, sites)
	p.placeAll(t.Context())
	if cond := spaceCondition(t, c, web); cond == nil {
		t.Fatal("no condition while the storage is short")
	}
	sites.set("lvm", 90)
	p.placeAll(t.Context())
	if cond := spaceCondition(t, c, web); cond != nil {
		t.Errorf("%s = %+v once 90 GiB are free", StorageLacksSpaceCondition, cond)
	}
}
