package provisioner

import (
	"context"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	m.Spec.InfrastructureRef.Name = name
	m.Status.Initialization.InfrastructureProvisioned = ptr.To(made)
	return m
}

// clonedVM is the ProxmoxMachine of web's machine name once capmox has cloned its VM.
func clonedVM(name string, vmid int64) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace("team-a")
	pm.SetName(name)
	pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web"})
	_ = unstructured.SetNestedField(pm.Object, vmid, "spec", "virtualMachineID")
	return pm
}

func placedOnSpace(t *testing.T, sites *spaceSites, objs ...client.Object) (*Placer, client.Client, *clusterv1.Cluster) {
	t.Helper()
	return placeOnSpace(t, diskCluster(t), sites, objs...)
}

func placeOnSpace(t *testing.T, web *clusterv1.Cluster, sites *spaceSites, objs ...client.Object) (*Placer, client.Client, *clusterv1.Cluster) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(proxmoxMachineGVK.GroupVersion().WithKind("ProxmoxMachineList"), &unstructured.UnstructuredList{})
	class := &clusterv1.ClusterClass{ObjectMeta: metav1.ObjectMeta{Name: "kubehenge-rke2", Namespace: "kubehenge-system"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{class, web}, objs...)...).
		WithStatusSubresource(&clusterv1.Cluster{}).Build()
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

// Mid-create, every VM is cloned and none provisioned yet: their disks hold their space already, so
// a thick storage left nearly full by them is not short.
func TestPlacerCountsClonedVMsAsMade(t *testing.T) {
	t.Parallel()
	sites := &spaceSites{}
	sites.set("lvm", 10)
	p, c, web := placedOnSpace(t, sites,
		webMachine("web-cp-a", "", false), webMachine("web-md-a", "md-0", false), webMachine("web-md-b", "md-0", false),
		clonedVM("web-cp-a", 101), clonedVM("web-md-a", 102), clonedVM("web-md-b", 103))
	p.placeAll(t.Context())
	if cond := spaceCondition(t, c, web); cond != nil {
		t.Errorf("%s = %+v while every VM is cloned", StorageLacksSpaceCondition, cond)
	}
}

// autoscaledOnSpace places web with its pool md-0 autoscaled from 2 to 10 workers, running at 2 with
// its control plane, every VM made.
func autoscaledOnSpace(t *testing.T, sites *spaceSites) (*Placer, client.Client, *clusterv1.Cluster, *events.FakeRecorder) {
	t.Helper()
	web := diskCluster(t)
	md := &web.Spec.Topology.Workers.MachineDeployments[0]
	md.Replicas = nil
	md.Metadata.Annotations = map[string]string{AutoscalerMinAnnotation: "2", AutoscalerMaxAnnotation: "10"}
	p, c, web := placeOnSpace(t, web, sites,
		webMachine("web-cp-a", "", true), webMachine("web-md-a", "md-0", true), webMachine("web-md-b", "md-0", true))
	rec := events.NewFakeRecorder(10)
	p.Events = rec
	return p, c, web, rec
}

// An autoscaled pool at its minimum with nothing waiting runs as asked: the workers it may grow to
// are not machines still to be made.
func TestPlacerLeavesAnAutoscaledPoolAtRestAlone(t *testing.T) {
	t.Parallel()
	sites := &spaceSites{}
	sites.set("lvm", 100)
	p, c, web, _ := autoscaledOnSpace(t, sites)
	p.placeAll(t.Context())
	if cond := spaceCondition(t, c, web); cond != nil {
		t.Errorf("%s = %+v for a pool at 2 of 2 to 10 with nothing waiting", StorageLacksSpaceCondition, cond)
	}
}

// A maximum the storage can never hold is said once in the autoscaler's warning, not as a fault on
// the cluster, and said again once it has gone and come back.
func TestPlacerWarnsOnceThatTheAutoscaledMaximumLacksRoom(t *testing.T) {
	t.Parallel()
	sites := &spaceSites{}
	sites.set("lvm", 100)
	p, c, web, rec := autoscaledOnSpace(t, sites)
	p.placeAll(t.Context())
	p.placeAll(t.Context())
	want := "Warning AutoscalerNoRoom Storage local-lvm (on pve1) reserves each disk's whole size and has 100 GiB free. " +
		"At the autoscaler's maximum, the machines still to be made need 240 GiB. A worker past that room waits, and the autoscaler gives it up after 30 minutes"
	got := recorded(rec)
	if len(got) != 1 || got[0] != want {
		t.Errorf("events %q, want once %q", got, want)
	}
	if cond := spaceCondition(t, c, web); cond != nil {
		t.Errorf("%s = %+v for a maximum the autoscaler may never reach", StorageLacksSpaceCondition, cond)
	}
	sites.set("lvm", 240)
	p.placeAll(t.Context())
	if got := recorded(rec); len(got) != 0 {
		t.Errorf("events %q once the maximum fits", got)
	}
	sites.set("lvm", 100)
	p.placeAll(t.Context())
	if got := recorded(rec); len(got) != 1 {
		t.Errorf("events %q once the room is short again, want one", got)
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
