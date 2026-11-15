package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// localVolume is a bound volume of the CSI plugin on storage, kept to node, for claim db/<claim>.
func localVolume(claim, node, storage string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		Name: "pvc-" + claim,
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: csiDriver, VolumeHandle: "cluster/" + node + "/" + storage + "/vm-9999-pvc-" + claim,
			}},
			ClaimRef: &corev1.ObjectReference{Namespace: "db", Name: claim},
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
			}}}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

// poolWorker is a ProxmoxMachine of web's pool in MachineSet set, its VM on node, or not cloned
// yet where node is empty.
func poolWorker(name, pool, set, node string) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace("team-a")
	pm.SetName(name)
	pm.SetLabels(map[string]string{
		clusterv1.ClusterNameLabel: "web", clusterv1.MachineDeploymentNameLabel: pool, clusterv1.MachineSetNameLabel: set,
	})
	if node != "" {
		_ = unstructured.SetNestedField(pm.Object, node, "status", "proxmoxNode")
	}
	return pm
}

// worker is a ProxmoxMachine of web's pool web-md-0.
func worker(name, set, node string) *unstructured.Unstructured {
	return poolWorker(name, "web-md-0", set, node)
}

// controlPlaneVM is a ProxmoxMachine of web's control plane, its VM on node.
func controlPlaneVM(name, node string) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace("team-a")
	pm.SetName(name)
	pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web", clusterv1.MachineControlPlaneLabel: ""})
	_ = unstructured.SetNestedField(pm.Object, node, "status", "proxmoxNode")
	return pm
}

// machineOf is the Machine of a ProxmoxMachine of web, with conditions.
func machineOf(pm *unstructured.Unstructured, conditions ...metav1.Condition) *clusterv1.Machine {
	m := &clusterv1.Machine{Namespace: "team-a", Name: pm.GetName(), Labels: map[string]string{clusterv1.ClusterNameLabel: "web"}}
	m.Spec.ClusterName = "web"
	m.Spec.InfrastructureRef.Name = pm.GetName()
	m.Status.Conditions = conditions
	return m
}

// deleting marks an object as being deleted, held by a finalizer.
func deleting[T client.Object](o T) T {
	o.SetDeletionTimestamp(new(metav1.Now()))
	o.SetFinalizers([]string{"e2e.kubehenge.dev/hold"})
	return o
}

func testCondition(kind string, status metav1.ConditionStatus) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: "Test", LastTransitionTime: metav1.Now()}
}

func volumeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(proxmoxMachineGVK.GroupVersion().WithKind("ProxmoxMachineList"), &unstructured.UnstructuredList{})
	return scheme
}

func workloadOf(t *testing.T, volumes ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(volumes...).Build()
}

// volumePlacer is a Placer of the clusters on local-lvm, or on storage as site describes it, whose
// workload clusters all hold volumes, with three nodes in the Proxmox VE cluster of which online
// are up.
func volumePlacer(t *testing.T, clusters []client.Object, site StorageSite, online Nodes, volumes []client.Object, objs ...client.Object) (*Placer, client.Client, *int) {
	t.Helper()
	class := &clusterv1.ClusterClass{Name: "kubehenge-rke2", Namespace: "kubehenge-system"}
	c := fake.NewClientBuilder().WithScheme(volumeScheme(t)).WithObjects(slices.Concat([]client.Object{class}, clusters, objs)...).
		WithStatusSubresource(&clusterv1.Cluster{}).Build()
	workload := workloadOf(t, volumes...)
	reads := 0
	if site.Nodes == nil {
		site.Nodes = []string{"pve1", "pve2", "pve3"}
	}
	if site.Active == nil {
		site.Active = site.Nodes
	}
	p := &Placer{
		Client: c, ClassNamespace: "kubehenge-system", Nodes: online,
		Sites: fakeSites{sites: &Sites{Storages: map[string]StorageSite{"local-lvm": site}}},
		Workload: func(context.Context, *clusterv1.Cluster) (client.Reader, bool, error) {
			reads++
			return workload, true, nil
		},
	}
	return p, c, &reads
}

// placeRound runs one round of the Placer and waits for its volume reads.
func placeRound(t *testing.T, p *Placer) {
	t.Helper()
	p.placeAll(t.Context())
	p.volumes.Wait()
}

// volumeCluster is web on local-lvm with a pool of two workers and its control plane up.
func volumeCluster(t *testing.T, variables ...clusterv1.ClusterVariable) *clusterv1.Cluster {
	t.Helper()
	web := cluster(append([]clusterv1.ClusterVariable{clusterVariable(t, "storage", "local-lvm")}, variables...)...)
	web.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: ptr.To[int32](2)}}
	web.Status.Conditions = []metav1.Condition{testCondition(clusterv1.ClusterControlPlaneInitializedCondition, metav1.ConditionTrue)}
	return web
}

func readCluster(t *testing.T, c client.Client, name string) *clusterv1.Cluster {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func volumesCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(readCluster(t, c, "web").Status.Conditions, VolumesWithoutWorkerCondition)
}

func allUp() *fakeNodes { return &fakeNodes{online: []string{"pve1", "pve2", "pve3"}} }

// The Placer records the nodes that hold the cluster's volumes, the one with the most first, which
// the webhook keeps a worker on. Each with its worker, nothing is wrong.
func TestPlacerRecordsTheNodesHoldingVolumes(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{
		localVolume("data-0", "pve2", "local-lvm"), localVolume("data-1", "pve3", "local-lvm"),
		localVolume("data-2", "pve3", "local-lvm"), localVolume("other", "pve1", "ceph"),
	}
	p, c, _ := volumePlacer(t, []client.Object{volumeCluster(t)}, StorageSite{}, allUp(), volumes,
		worker("web-a", "ms-1", "pve2"), worker("web-b", "ms-1", "pve3"))
	placeRound(t, p)
	if v := readCluster(t, c, "web").Annotations[VolumeNodesAnnotation]; v != "pve3,pve2" {
		t.Errorf("records %q, want the nodes with volumes on the cluster's storage, the most first", v)
	}
	if cond := volumesCondition(t, c); cond != nil {
		t.Errorf("every volume node has a worker, and the cluster says %q", cond.Message)
	}
}

// slowClusters are two clusters on local-lvm, a-slow, whose workload cluster answers only after
// wait or never, and b-fast.
func slowClusters(t *testing.T, wait time.Duration) (*Placer, client.Client) {
	t.Helper()
	slow, fast := volumeCluster(t), volumeCluster(t)
	slow.Name, fast.Name = "a-slow", "b-fast"
	p, c, _ := volumePlacer(t, []client.Object{slow, fast}, StorageSite{}, allUp(), nil)
	workload := workloadOf(t, localVolume("data-0", "pve1", "local-lvm"))
	p.Workload = func(ctx context.Context, cluster *clusterv1.Cluster) (client.Reader, bool, error) {
		if cluster.Name == "b-fast" {
			return workload, true, nil
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		return nil, false, errors.New("dial tcp: i/o timeout")
	}
	return p, c
}

// A workload cluster that does not answer holds up neither the record of the nodes online nor
// another cluster's volumes.
func TestPlacerRecordsTheOnlineNodesBesideAClusterThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	p, c := slowClusters(t, 3*time.Second)
	p.VolumeTimeout = 5 * time.Second
	start := time.Now()
	p.placeAll(t.Context())
	if took := time.Since(start); took > time.Second {
		t.Errorf("the placement round took %s", took)
	}
	for _, name := range []string{"a-slow", "b-fast"} {
		if v := readCluster(t, c, name).Annotations[OnlineNodesAnnotation]; v != "pve1,pve2,pve3" {
			t.Errorf("right after the round %s records %q", name, v)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for readCluster(t, c, "b-fast").Annotations[VolumeNodesAnnotation] != "pve1" {
		if time.Now().After(deadline) {
			t.Fatal("b-fast's volumes waited on a-slow")
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.volumes.Wait()
}

// A read of one cluster's volumes ends on the Placer's own timeout, the next round's reads with it.
func TestPlacerGivesUpOnVolumesAfterItsTimeout(t *testing.T) {
	t.Parallel()
	p, c := slowClusters(t, time.Hour)
	p.VolumeTimeout = 100 * time.Millisecond
	p.placeAll(t.Context())
	done := make(chan struct{})
	go func() { p.volumes.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the volume reads ran past their timeout")
	}
	if v, ok := readCluster(t, c, "a-slow").Annotations[VolumeNodesAnnotation]; ok {
		t.Errorf("a-slow records %q without an answer", v)
	}
}

// On shared storage every worker reaches every volume, so the Placer reads none and records nothing.
func TestPlacerLeavesSharedStorageAlone(t *testing.T) {
	t.Parallel()
	web := volumeCluster(t)
	web.Annotations = map[string]string{VolumeNodesAnnotation: "pve2"}
	p, c, reads := volumePlacer(t, []client.Object{web}, StorageSite{Shared: true}, allUp(),
		[]client.Object{localVolume("data-0", "pve2", "local-lvm")})
	placeRound(t, p)
	if *reads != 0 {
		t.Errorf("read the workload cluster %d times for shared storage", *reads)
	}
	if v, ok := readCluster(t, c, "web").Annotations[VolumeNodesAnnotation]; ok {
		t.Errorf("kept the volume nodes %q on shared storage", v)
	}
}

// A volume's node that is offline or that the cluster may not use takes no worker: the condition
// names the volumes and the fix.
func TestVolumesWithoutWorkerWhereTheNodeCannotTakeOne(t *testing.T) {
	t.Parallel()
	web := volumeCluster(t, clusterVariable(t, allowedNodesVariable, []string{"pve1", "pve3"}))
	volumes := []client.Object{localVolume("data-0", "pve2", "local-lvm"), localVolume("data-1", "pve3", "local-lvm")}
	p, c, _ := volumePlacer(t, []client.Object{web}, StorageSite{}, &fakeNodes{online: []string{"pve1", "pve2"}}, volumes, worker("web-a", "ms-1", "pve1"))
	placeRound(t, p)
	cond := volumesCondition(t, c)
	if cond == nil {
		t.Fatal("no condition for volumes whose nodes take no worker")
	}
	want := "Volume db/data-0 is on node pve2, which this cluster may not use. Add pve2 to allowedNodes, or move it with pvecsictl migrate. " +
		"Volume db/data-1 is on node pve3, which is offline. Bring pve3 back online."
	if cond.Reason != reasonNodeNotAllowed || cond.Message != want {
		t.Errorf("%s: %q", cond.Reason, cond.Message)
	}
}

// With fewer workers than nodes holding volumes, some volumes go without: the condition says to
// add workers.
func TestVolumesWithoutWorkerWhenThePoolsAreTooSmall(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{
		localVolume("data-0", "pve1", "local-lvm"), localVolume("data-1", "pve2", "local-lvm"), localVolume("data-2", "pve3", "local-lvm"),
	}
	p, c, _ := volumePlacer(t, []client.Object{volumeCluster(t)}, StorageSite{}, allUp(), volumes,
		worker("web-a", "ms-1", "pve1"), worker("web-b", "ms-1", "pve2"))
	placeRound(t, p)
	cond := volumesCondition(t, c)
	if cond == nil {
		t.Fatal("no condition for a volume node left without a worker")
	}
	want := "Volume db/data-2 is on node pve3, which has no worker. Volumes are on 3 nodes and the cluster has 2 workers, so add workers to a pool."
	if cond.Reason != reasonTooFewWorkers || cond.Message != want {
		t.Errorf("%s: %q", cond.Reason, cond.Message)
	}
}

// A cluster that asks for no workers runs its pods on its control planes, which cover their nodes.
// A node with volumes and no control plane is not a shortage of workers.
func TestControlPlanesRunTheVolumesOfAClusterWithoutWorkers(t *testing.T) {
	t.Parallel()
	web := volumeCluster(t)
	web.Spec.Topology.Workers.MachineDeployments = nil
	held := localVolume("data-0", "pve1", "local-lvm")
	p, c, _ := volumePlacer(t, []client.Object{web}, StorageSite{}, allUp(), []client.Object{held}, controlPlaneVM("web-cp-a", "pve1"))
	placeRound(t, p)
	if cond := volumesCondition(t, c); cond != nil {
		t.Errorf("the control plane on pve1 runs the volume's pod, and the cluster says %s: %q", cond.Reason, cond.Message)
	}
	web = volumeCluster(t)
	web.Spec.Topology.Workers.MachineDeployments = nil
	volumes := []client.Object{held, localVolume("data-1", "pve2", "local-lvm")}
	p, c, _ = volumePlacer(t, []client.Object{web}, StorageSite{}, allUp(), volumes, controlPlaneVM("web-cp-a", "pve1"))
	placeRound(t, p)
	cond := volumesCondition(t, c)
	want := "Volume db/data-1 is on node pve2, where no control plane runs. Add a worker pool, or move it with pvecsictl migrate."
	if cond == nil || cond.Reason != reasonNoWorker || cond.Message != want {
		t.Errorf("the condition is %+v", cond)
	}
}

// A volume kept to a node that left the Proxmox VE cluster is reported, and no worker is sent there.
func TestVolumeOnANodeThatIsGoneIsReported(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{localVolume("data-0", "pve1", "local-lvm"), localVolume("data-9", "pve9", "local-lvm")}
	p, c, _ := volumePlacer(t, []client.Object{volumeCluster(t)}, StorageSite{}, allUp(), volumes, worker("web-a", "ms-1", "pve1"))
	placeRound(t, p)
	if v := readCluster(t, c, "web").Annotations[VolumeNodesAnnotation]; v != "pve1" {
		t.Errorf("records %q, want only the node in the Proxmox VE cluster", v)
	}
	cond := volumesCondition(t, c)
	want := "Volume db/data-9 is on node pve9, which is not in the Proxmox VE cluster. Restore it from a backup or delete its claim."
	if cond == nil || cond.Reason != reasonNodeGone || cond.Message != want {
		t.Errorf("the condition is %+v", cond)
	}
}

// The advice for a node with volumes and no worker names a worker whose deletion uncovers no other
// node with volumes, and the webhook sends its replacement to the node named.
func TestNoWorkerAdviceNamesAWorkerWhoseReplacementGoesThere(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{
		localVolume("data-0", "pve1", "local-lvm"), localVolume("data-1", "pve1", "local-lvm"), localVolume("data-2", "pve2", "local-lvm"),
	}
	p, c, _ := volumePlacer(t, []client.Object{volumeCluster(t)}, StorageSite{}, allUp(), volumes,
		worker("web-a", "ms-1", "pve1"), worker("web-b", "ms-1", "pve3"))
	placeRound(t, p)
	got := readCluster(t, c, "web")
	cond := meta.FindStatusCondition(got.Status.Conditions, VolumesWithoutWorkerCondition)
	want := "Volume db/data-2 is on node pve2, which has no worker. Delete machine web-b on node pve3, and its replacement goes to pve2."
	if cond == nil || cond.Reason != reasonNoWorker || cond.Message != want {
		t.Fatalf("the condition is %+v", cond)
	}
	// After the deletion the MachineSet makes the replacement beside web-a.
	if pinned := placeWorker(t, got, worker("web-c", "ms-1", ""), worker("web-a", "ms-1", "pve1")); !slices.Equal(pinned, []string{"pve2"}) {
		t.Errorf("the replacement of web-b is kept to %v, not to pve2 as the advice says", pinned)
	}
}

// With no worker on a node without volumes, the advice names the second worker on a node with
// volumes, and the webhook sends its replacement to the node named.
func TestNoWorkerAdviceNamesASecondWorkerOnAVolumeNode(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{
		localVolume("data-0", "pve1", "local-lvm"), localVolume("data-1", "pve1", "local-lvm"), localVolume("data-2", "pve2", "local-lvm"),
	}
	p, c, _ := volumePlacer(t, []client.Object{volumeCluster(t)}, StorageSite{}, allUp(), volumes,
		worker("web-a", "ms-1", "pve1"), worker("web-b", "ms-1", "pve1"))
	placeRound(t, p)
	got := readCluster(t, c, "web")
	cond := meta.FindStatusCondition(got.Status.Conditions, VolumesWithoutWorkerCondition)
	want := "Volume db/data-2 is on node pve2, which has no worker. Delete machine web-b on node pve1, and its replacement goes to pve2."
	if cond == nil || cond.Reason != reasonNoWorker || cond.Message != want {
		t.Fatalf("the condition is %+v", cond)
	}
	if pinned := placeWorker(t, got, worker("web-c", "ms-1", ""), worker("web-a", "ms-1", "pve1")); !slices.Equal(pinned, []string{"pve2"}) {
		t.Errorf("the replacement of web-b is kept to %v, not to pve2 as the advice says", pinned)
	}
}

// A node with volumes that no new VM of the cluster may go to now gets no advice to send one there.
func TestNoWorkerAdviceLeavesOutNodesThatTakeNoVM(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{localVolume("data-0", "pve1", "local-lvm"), localVolume("data-2", "pve2", "local-lvm")}
	inactive := &fakeNodes{online: []string{"pve1", "pve2", "pve3"}, inactive: map[string][]string{"pve2": {"local-lvm"}}}
	p, c, _ := volumePlacer(t, []client.Object{volumeCluster(t)}, StorageSite{Active: []string{"pve1", "pve3"}}, inactive, volumes,
		worker("web-a", "ms-1", "pve1"), worker("web-b", "ms-1", "pve3"))
	placeRound(t, p)
	cond := volumesCondition(t, c)
	want := "Volume db/data-2 is on node pve2, where storage local-lvm is not active. Activate local-lvm on pve2."
	if cond == nil || cond.Reason != reasonStorageInactive || cond.Message != want {
		t.Errorf("with the storage inactive on pve2, the condition is %+v", cond)
	}
}

// copyObjects are copies of objs, for subtests that run beside each other to build clients of.
func copyObjects(t *testing.T, objs []client.Object) []client.Object {
	t.Helper()
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		c, ok := o.DeepCopyObject().(client.Object)
		if !ok {
			t.Fatalf("a copy of %T is no object", o)
		}
		out = append(out, c)
	}
	return out
}

// machineSizes give the cluster's machines their size, its workers 8 GiB of memory.
func machineSizes(t *testing.T) []clusterv1.ClusterVariable {
	t.Helper()
	return []clusterv1.ClusterVariable{
		clusterVariable(t, "controlPlaneMachine", map[string]int{"cores": 2, "memoryMiB": 4096, "diskGiB": 30}),
		clusterVariable(t, "workerMachine", map[string]int{"cores": 2, "memoryMiB": 8192, "diskGiB": 30}),
	}
}

// nodeList are online nodes as Proxmox VE reports them.
type nodeList []Node

func (n nodeList) OnlineNodes(context.Context) ([]Node, error) { return slices.Clone(n), nil }

// threeUp are pve1 to pve3 online, pve2 as change makes it.
func threeUp(change func(*Node)) nodeList {
	nodes := nodeList{{Name: "pve1", CPULevel: 3}, {Name: "pve2", CPULevel: 3}, {Name: "pve3", CPULevel: 3}}
	change(&nodes[1])
	return nodes
}

// For each thing that keeps the cluster's new VMs off a node with volumes, the condition names it
// and its fix.
func TestVolumesWithoutWorkerSayWhatKeepsTheVMsOffTheirNode(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{
		localVolume("data-0", "pve1", "local-lvm"), localVolume("data-1", "pve1", "local-lvm"), localVolume("data-2", "pve2", "local-lvm"),
	}
	all := []string{"pve1", "pve2", "pve3"}
	big := machineSizes(t)
	for _, tc := range []struct {
		name      string
		variables []clusterv1.ClusterVariable
		nodes     Nodes
		sites     func(*clusterv1.Cluster, *Sites)
		reason    string
		want      string
	}{
		{
			name: "offline", nodes: &fakeNodes{online: []string{"pve1", "pve3"}}, reason: reasonNodeOffline,
			want: "which is offline. Bring pve2 back online.",
		},
		{
			name: "maintenance", nodes: &fakeNodes{online: all, maintenance: []string{"pve2"}}, reason: reasonNodeMaintenance,
			want: "which is in maintenance. Their pods start once pve2 leaves maintenance.",
		},
		{
			name: "CPU level", nodes: &fakeNodes{online: all, levels: map[string]int{"pve2": 0}}, reason: reasonNodeUnavailable,
			want: "whose CPU does not reach x86-64-v2, which the cluster's VMs need. Move it with pvecsictl migrate.",
		},
		{
			name: "bridge", variables: []clusterv1.ClusterVariable{clusterVariable(t, networkVariable, map[string]string{"bridge": "vmbr1"})},
			nodes: allUp(), reason: reasonNodeUnavailable,
			sites: func(_ *clusterv1.Cluster, s *Sites) {
				s.Bridges = map[string][]string{"pve1": {"vmbr1"}, "pve2": {"vmbr0"}, "pve3": {"vmbr1"}}
			},
			want: "which has no bridge or VNet vmbr1. Add vmbr1 to the node.",
		},
		{
			name: "template reach", nodes: allUp(), reason: reasonNodeUnavailable,
			sites: func(c *clusterv1.Cluster, s *Sites) {
				s.Templates = []TemplateSite{templateSiteOf(t, c, "pve1", "", "local-lvm")}
			},
			want: "which the cluster's templates cannot be cloned to. Build the cluster's templates on shared storage.",
		},
		{
			name: "memory", variables: big, reason: reasonNodeUnavailable,
			nodes: threeUp(func(n *Node) { n.FreeMiB, n.MemoryKnown = 2048, true }),
			want:  "which has 2 GiB of memory free, too little for the cluster's largest machine. Free memory on the node.",
		},
		{
			name: "newer major release", reason: reasonNodeUnavailable,
			nodes: nodeList{{Name: "pve1", CPULevel: 3, Major: 8}, {Name: "pve2", CPULevel: 3, Major: 9}, {Name: "pve3", CPULevel: 3, Major: 8}},
			want:  "which runs Proxmox VE 9 while nodes of an older release have room. Upgrade the other nodes to Proxmox VE 9.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			web := volumeCluster(t, tc.variables...)
			p, c, _ := volumePlacer(t, []client.Object{web}, StorageSite{}, tc.nodes, copyObjects(t, volumes),
				worker("web-a", "ms-1", "pve1"), worker("web-b", "ms-1", "pve3"))
			if tc.sites != nil {
				sites, ok := p.Sites.(fakeSites)
				if !ok {
					t.Fatalf("sites are a %T", p.Sites)
				}
				tc.sites(web, sites.sites)
			}
			placeRound(t, p)
			cond := volumesCondition(t, c)
			want := "Volume db/data-2 is on node pve2, " + tc.want
			if cond == nil || cond.Reason != tc.reason || cond.Message != want {
				t.Errorf("the condition is %+v, want %s: %q", cond, tc.reason, want)
			}
		})
	}
}

// A machine kept to a node from a record that was out of date is let go of within a round once the
// node is offline, before its VM is made. One whose VM is made keeps its node.
func TestPlacerUnpinsAMachineKeptToAnOfflineNode(t *testing.T) {
	t.Parallel()
	web := volumeCluster(t)
	web.Annotations = map[string]string{OnlineNodesAnnotation: "pve1,pve2,pve3", VolumeNodesAnnotation: "pve2"}
	waiting := worker("web-b", "ms-1", "")
	pinned := placeWorker(t, web, waiting, worker("web-a", "ms-1", "pve1"))
	if !slices.Equal(pinned, []string{"pve2"}) {
		t.Fatalf("the stale record kept the new worker to %v", pinned)
	}
	_ = unstructured.SetNestedStringSlice(waiting.Object, pinned, "spec", "allowedNodes")
	waiting.SetAnnotations(map[string]string{VolumePinAnnotation: "pve2"})
	cloned := worker("web-c", "ms-1", "pve2")
	_ = unstructured.SetNestedStringSlice(cloned.Object, []string{"pve2"}, "spec", "allowedNodes")
	cloned.SetAnnotations(map[string]string{VolumePinAnnotation: "pve2"})
	p, c, _ := volumePlacer(t, []client.Object{web}, StorageSite{}, &fakeNodes{online: []string{"pve1", "pve3"}}, nil,
		worker("web-a", "ms-1", "pve1"), waiting, cloned)
	placeRound(t, p)
	read := func(name string) *unstructured.Unstructured {
		pm := &unstructured.Unstructured{}
		pm.SetGroupVersionKind(proxmoxMachineGVK)
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, pm); err != nil {
			t.Fatal(err)
		}
		return pm
	}
	if allowed, found, _ := unstructured.NestedStringSlice(read("web-b").Object, "spec", "allowedNodes"); found {
		t.Errorf("pinned to %v while pve2 is offline", allowed)
	}
	if allowed, _, _ := unstructured.NestedStringSlice(read("web-c").Object, "spec", "allowedNodes"); !slices.Equal(allowed, []string{"pve2"}) {
		t.Errorf("a machine whose VM is made lost its node: %v", allowed)
	}
}

// placeWorker asks WorkerPlacement about a new ProxmoxMachine and returns the nodes it is kept to,
// nil where it is let through as it is.
func placeWorker(t *testing.T, web *clusterv1.Cluster, created *unstructured.Unstructured, objs ...client.Object) []string {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(volumeScheme(t)).WithObjects(append([]client.Object{web}, objs...)...).Build()
	raw, err := json.Marshal(created.Object)
	if err != nil {
		t.Fatal(err)
	}
	resp := (&WorkerPlacement{Reader: c}).Handle(t.Context(), admission.Request{
		Namespace: "team-a", Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw},
	})
	if !resp.Allowed {
		t.Fatalf("refused: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		return nil
	}
	if len(resp.Patches) != 2 || resp.Patches[0].Path != "/spec/allowedNodes" || resp.Patches[1].Path != "/metadata/annotations" {
		t.Fatalf("patches %+v", resp.Patches)
	}
	nodes, ok := resp.Patches[0].Value.([]string)
	if !ok {
		t.Fatalf("allowedNodes is a %T", resp.Patches[0].Value)
	}
	return nodes
}

func placedCluster(t *testing.T, volumeNodes string, variables ...clusterv1.ClusterVariable) *clusterv1.Cluster {
	t.Helper()
	web := volumeCluster(t, variables...)
	web.Annotations = map[string]string{OnlineNodesAnnotation: "pve1,pve2,pve3", VolumeNodesAnnotation: volumeNodes}
	return web
}

// In a rollout the old worker on a node with volumes still runs when its replacement is made, and
// the replacement goes to that node.
func TestWorkerPlacementSendsAReplacementToItsPredecessorsNode(t *testing.T) {
	t.Parallel()
	got := placeWorker(t, placedCluster(t, "pve2"), worker("web-new-b", "ms-2", ""),
		worker("web-old-a", "ms-1", "pve1"), worker("web-old-b", "ms-1", "pve2"), worker("web-new-a", "ms-2", "pve1"))
	if !slices.Equal(got, []string{"pve2"}) {
		t.Errorf("the replacement is kept to %v, want its predecessor's node pve2", got)
	}
}

// A growing pool fills the nodes with volumes and no worker first, the one with the most volumes
// first, and nodes the cluster may not use are passed over.
func TestWorkerPlacementPrefersVolumeNodesAsAPoolGrows(t *testing.T) {
	t.Parallel()
	running := worker("web-a", "ms-1", "pve1")
	if got := placeWorker(t, placedCluster(t, "pve3,pve2"), worker("web-b", "ms-1", ""), running); !slices.Equal(got, []string{"pve3"}) {
		t.Errorf("a new worker is kept to %v, want pve3, which holds the most volumes", got)
	}
	// A worker on its way to pve3 covers it.
	pending := worker("web-c", "ms-1", "")
	_ = unstructured.SetNestedStringSlice(pending.Object, []string{"pve3"}, "spec", "allowedNodes")
	if got := placeWorker(t, placedCluster(t, "pve3,pve2"), worker("web-b", "ms-1", ""), running, pending); !slices.Equal(got, []string{"pve2"}) {
		t.Errorf("with a worker on its way to pve3, a new worker is kept to %v, want pve2", got)
	}
	if got := placeWorker(t, placedCluster(t, "pve1"), worker("web-b", "ms-1", ""), running); got != nil {
		t.Errorf("with every volume node covered, a new worker is kept to %v", got)
	}
	pinned := placedCluster(t, "pve2", clusterVariable(t, allowedNodesVariable, []string{"pve1", "pve3"}))
	if got := placeWorker(t, pinned, worker("web-b", "ms-1", ""), running); got != nil {
		t.Errorf("a new worker is kept to %v, a node the cluster may not use", got)
	}
	// With every chosen node down the cluster stays kept to them, and no pin goes to one.
	down := placedCluster(t, "pve2", clusterVariable(t, allowedNodesVariable, []string{"pve2"}))
	down.Annotations[OnlineNodesAnnotation] = "pve1,pve3"
	if got := placeWorker(t, down, worker("web-b", "ms-1", ""), running); got != nil {
		t.Errorf("a new worker is kept to %v while it is offline", got)
	}
}

// A worker on its way out covers nothing: one out of date in another pool, one marked for
// remediation, one whose ProxmoxMachine or Machine is being deleted.
func TestWorkerPlacementPassesOverWorkersOnTheirWayOut(t *testing.T) {
	t.Parallel()
	web := placedCluster(t, "pve2")
	created := worker("web-new", "ms-1", "")
	other := poolWorker("web-other", "web-md-1", "ms-9", "pve2")
	same := worker("web-same", "ms-1", "pve2")
	cases := []struct {
		name string
		objs []client.Object
		want []string
	}{
		{"another pool's worker out of date", []client.Object{other, machineOf(other, testCondition(clusterv1.MachineUpToDateCondition, metav1.ConditionFalse))}, []string{"pve2"}},
		{"another pool's worker up to date", []client.Object{other, machineOf(other, testCondition(clusterv1.MachineUpToDateCondition, metav1.ConditionTrue))}, nil},
		{"a worker marked for remediation", []client.Object{same, machineOf(same, testCondition(clusterv1.MachineOwnerRemediatedCondition, metav1.ConditionFalse))}, []string{"pve2"}},
		{"a worker whose ProxmoxMachine is being deleted", []client.Object{deleting(same.DeepCopy())}, []string{"pve2"}},
		{"a worker whose Machine is being deleted", []client.Object{same, deleting(machineOf(same))}, []string{"pve2"}},
	}
	for _, tc := range cases {
		if got := placeWorker(t, web, created, tc.objs...); !slices.Equal(got, tc.want) {
			t.Errorf("%s on pve2: kept to %v, want %v", tc.name, got, tc.want)
		}
	}
}

// While a worker is on its way out, the advice names only a machine whose replacement the webhook
// sends to the node it names, and otherwise says what it waits for.
func TestNoWorkerAdviceFollowsTheWebhookWhileWorkersLeave(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{
		localVolume("data-0", "pve1", "local-lvm"), localVolume("data-1", "pve1", "local-lvm"), localVolume("data-2", "pve2", "local-lvm"),
	}
	outdated := testCondition(clusterv1.MachineUpToDateCondition, metav1.ConditionFalse)
	remediated := testCondition(clusterv1.MachineOwnerRemediatedCondition, metav1.ConditionFalse)
	other := poolWorker("web-a", "web-md-1", "ms-9", "pve1")
	top := worker("web-a", "ms-1", "pve1")
	spare := worker("web-b", "ms-1", "pve3")
	waits := "Volume db/data-2 is on node pve2, which has no worker. Machine web-a is on its way out. The fix shows here once it is replaced or gone."
	for _, tc := range []struct {
		name string
		objs []client.Object
		want string
		// named is the machine the advice names, and node where its replacement goes.
		named, node string
	}{
		{"another pool's worker out of date on the top node", []client.Object{other, machineOf(other, outdated), spare, machineOf(spare)}, waits, "", ""},
		{"a worker marked for remediation on the top node", []client.Object{top, machineOf(top, remediated), spare, machineOf(spare)}, waits, "", ""},
		{
			"a worker marked for remediation on a node without volumes",
			[]client.Object{
				worker("web-a", "ms-1", "pve1"), spare, machineOf(spare, remediated), worker("web-c", "ms-1", "pve3"),
			},
			"Volume db/data-2 is on node pve2, which has no worker. Delete machine web-c on node pve3, and its replacement goes to pve2.",
			"web-c", "pve2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			own := func() []client.Object { return copyObjects(t, tc.objs) }
			p, c, _ := volumePlacer(t, []client.Object{volumeCluster(t)}, StorageSite{}, allUp(), copyObjects(t, volumes), own()...)
			placeRound(t, p)
			got := readCluster(t, c, "web")
			cond := meta.FindStatusCondition(got.Status.Conditions, VolumesWithoutWorkerCondition)
			if cond == nil || cond.Reason != reasonNoWorker || cond.Message != tc.want {
				t.Fatalf("the condition is %+v, want %q", cond, tc.want)
			}
			// Where the advice waits, the webhook sends the spare's replacement to the top node.
			gone := own()
			name, node := tc.named, tc.node
			if name == "" {
				name, node = "web-b", "pve1"
			}
			gone = append(gone, deleting(machineOf(worker(name, "ms-1", ""))))
			gone = slices.DeleteFunc(gone, func(o client.Object) bool {
				_, isMachine := o.(*clusterv1.Machine)
				return isMachine && o.GetName() == name && o.GetDeletionTimestamp().IsZero()
			})
			if pinned := placeWorker(t, got, worker("web-new", "ms-1", ""), gone...); !slices.Equal(pinned, []string{node}) {
				t.Errorf("the replacement of %s is kept to %v, want %s", name, pinned, node)
			}
		})
	}
}

// A pin outlives the record of the nodes with volumes: once its node is offline it is taken off,
// whatever the record holds.
func TestPlacerUnpinsWhateverTheVolumeRecordHolds(t *testing.T) {
	t.Parallel()
	web := volumeCluster(t)
	web.Annotations = map[string]string{OnlineNodesAnnotation: "pve1,pve2,pve3", VolumeNodesAnnotation: "pve2"}
	waiting := worker("web-b", "ms-1", "")
	_ = unstructured.SetNestedStringSlice(waiting.Object, []string{"pve2"}, "spec", "allowedNodes")
	waiting.SetAnnotations(map[string]string{VolumePinAnnotation: "pve2"})
	nodes := allUp()
	p, c, _ := volumePlacer(t, []client.Object{web}, StorageSite{}, nodes, nil, worker("web-a", "ms-1", "pve1"), waiting)
	read := func() *unstructured.Unstructured {
		pm := &unstructured.Unstructured{}
		pm.SetGroupVersionKind(proxmoxMachineGVK)
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web-b"}, pm); err != nil {
			t.Fatal(err)
		}
		return pm
	}
	placeRound(t, p)
	if v, has := readCluster(t, c, "web").Annotations[VolumeNodesAnnotation]; has {
		t.Fatalf("with no volumes the record holds %q", v)
	}
	if pin := read().GetAnnotations()[VolumePinAnnotation]; pin != "pve2" {
		t.Fatalf("with pve2 online the pin is %q", pin)
	}
	nodes.online = []string{"pve1", "pve3"}
	placeRound(t, p)
	pm := read()
	if allowed, found, _ := unstructured.NestedStringSlice(pm.Object, "spec", "allowedNodes"); found {
		t.Errorf("pinned to %v while pve2 is offline", allowed)
	}
	if pin, has := pm.GetAnnotations()[VolumePinAnnotation]; has {
		t.Errorf("the pin %q stays", pin)
	}
}

// A cluster deleted while its volumes are read logs no error.
func TestPlacerSaysNothingOfAClusterDeletedDuringItsRead(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var errs []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(args, `"error"`) {
			errs = append(errs, prefix+args)
		}
	}, funcr.Options{})
	web := volumeCluster(t, machineSizes(t)...)
	p, c, _ := volumePlacer(t, []client.Object{web}, StorageSite{}, allUp(), nil, worker("web-a", "ms-1", "pve1"))
	workload := workloadOf(t, localVolume("data-0", "pve2", "local-lvm"))
	p.Workload = func(ctx context.Context, cluster *clusterv1.Cluster) (client.Reader, bool, error) {
		if err := c.Delete(ctx, cluster.DeepCopy()); err != nil {
			return nil, false, err
		}
		return workload, true, nil
	}
	p.placeAll(logr.NewContext(t.Context(), log))
	p.volumes.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Errorf("logged %v", errs)
	}
}
