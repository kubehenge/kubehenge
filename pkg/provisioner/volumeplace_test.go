package provisioner

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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

// worker is a ProxmoxMachine of web's pool md-0 in MachineSet set, its VM on node, or not cloned yet
// where node is empty.
func worker(name, set, node string) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace("team-a")
	pm.SetName(name)
	pm.SetLabels(map[string]string{
		clusterv1.ClusterNameLabel: "web", clusterv1.MachineDeploymentNameLabel: "web-md-0", clusterv1.MachineSetNameLabel: set,
	})
	if node != "" {
		_ = unstructured.SetNestedField(pm.Object, node, "status", "proxmoxNode")
	}
	return pm
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

// volumePlacer is a Placer of web on local-lvm, or on storage as site describes it, whose workload
// cluster holds volumes, with three nodes in the Proxmox VE cluster of which online are up.
func volumePlacer(t *testing.T, web *clusterv1.Cluster, site StorageSite, online []string, volumes []client.Object, objs ...client.Object) (*Placer, client.Client, *int) {
	t.Helper()
	class := &clusterv1.ClusterClass{Name: "kubehenge-rke2", Namespace: "kubehenge-system"}
	c := fake.NewClientBuilder().WithScheme(volumeScheme(t)).WithObjects(append([]client.Object{class, web}, objs...)...).
		WithStatusSubresource(&clusterv1.Cluster{}).Build()
	workloadScheme := runtime.NewScheme()
	if err := corev1.AddToScheme(workloadScheme); err != nil {
		t.Fatal(err)
	}
	workload := fake.NewClientBuilder().WithScheme(workloadScheme).WithObjects(volumes...).Build()
	reads := 0
	if site.Nodes == nil {
		site.Nodes = []string{"pve1", "pve2", "pve3"}
	}
	p := &Placer{
		Client: c, ClassNamespace: "kubehenge-system", Nodes: &fakeNodes{online: online},
		Sites: fakeSites{sites: &Sites{Storages: map[string]StorageSite{"local-lvm": site}}},
		Workload: func(context.Context, *clusterv1.Cluster) (client.Reader, bool, error) {
			reads++
			return workload, true, nil
		},
	}
	return p, c, &reads
}

func volumeCluster(t *testing.T, variables ...clusterv1.ClusterVariable) *clusterv1.Cluster {
	t.Helper()
	return cluster(append([]clusterv1.ClusterVariable{clusterVariable(t, "storage", "local-lvm")}, variables...)...)
}

func readCluster(t *testing.T, c client.Client) *clusterv1.Cluster {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

var allUp = []string{"pve1", "pve2", "pve3"}

// The Placer records the nodes that hold the cluster's volumes, the one with the most first, which
// the webhook keeps a worker on. Each with its worker, nothing is wrong.
func TestPlacerRecordsTheNodesHoldingVolumes(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{
		localVolume("data-0", "pve2", "local-lvm"), localVolume("data-1", "pve3", "local-lvm"),
		localVolume("data-2", "pve3", "local-lvm"), localVolume("other", "pve1", "ceph"),
	}
	p, c, _ := volumePlacer(t, volumeCluster(t), StorageSite{}, allUp, volumes,
		worker("web-a", "ms-1", "pve2"), worker("web-b", "ms-1", "pve3"))
	p.placeAll(t.Context())
	got := readCluster(t, c)
	if v := got.Annotations[VolumeNodesAnnotation]; v != "pve3,pve2" {
		t.Errorf("records %q, want the nodes with volumes on the cluster's storage, the most first", v)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, VolumesWithoutWorkerCondition); cond != nil {
		t.Errorf("every volume node has a worker, and the cluster says %q", cond.Message)
	}
}

// On shared storage every worker reaches every volume, so the Placer reads none and records nothing.
func TestPlacerLeavesSharedStorageAlone(t *testing.T) {
	t.Parallel()
	web := volumeCluster(t)
	web.Annotations = map[string]string{VolumeNodesAnnotation: "pve2"}
	p, c, reads := volumePlacer(t, web, StorageSite{Shared: true}, allUp,
		[]client.Object{localVolume("data-0", "pve2", "local-lvm")})
	p.placeAll(t.Context())
	if *reads != 0 {
		t.Errorf("read the workload cluster %d times for shared storage", *reads)
	}
	if v, ok := readCluster(t, c).Annotations[VolumeNodesAnnotation]; ok {
		t.Errorf("kept the volume nodes %q on shared storage", v)
	}
}

// A volume's node that is offline or that the cluster may not use takes no worker: the condition
// names the volumes and the fix.
func TestVolumesWithoutWorkerWhereTheNodeCannotTakeOne(t *testing.T) {
	t.Parallel()
	web := volumeCluster(t, clusterVariable(t, allowedNodesVariable, []string{"pve1", "pve3"}))
	volumes := []client.Object{localVolume("data-0", "pve2", "local-lvm"), localVolume("data-1", "pve3", "local-lvm")}
	p, c, _ := volumePlacer(t, web, StorageSite{}, []string{"pve1", "pve2"}, volumes, worker("web-a", "ms-1", "pve1"))
	p.placeAll(t.Context())
	cond := meta.FindStatusCondition(readCluster(t, c).Status.Conditions, VolumesWithoutWorkerCondition)
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
	p, c, _ := volumePlacer(t, volumeCluster(t), StorageSite{}, allUp, volumes,
		worker("web-a", "ms-1", "pve1"), worker("web-b", "ms-1", "pve2"))
	p.placeAll(t.Context())
	cond := meta.FindStatusCondition(readCluster(t, c).Status.Conditions, VolumesWithoutWorkerCondition)
	if cond == nil {
		t.Fatal("no condition for a volume node left without a worker")
	}
	want := "Volume db/data-2 is on node pve3, which has no worker. Volumes are on 3 nodes and the cluster has 2 workers, so add workers to a pool."
	if cond.Reason != reasonTooFewWorkers || cond.Message != want {
		t.Errorf("%s: %q", cond.Reason, cond.Message)
	}
}

// A volume kept to a node that left the Proxmox VE cluster is reported, and no worker is sent there.
func TestVolumeOnANodeThatIsGoneIsReported(t *testing.T) {
	t.Parallel()
	volumes := []client.Object{localVolume("data-0", "pve1", "local-lvm"), localVolume("data-9", "pve9", "local-lvm")}
	p, c, _ := volumePlacer(t, volumeCluster(t), StorageSite{}, allUp, volumes, worker("web-a", "ms-1", "pve1"))
	p.placeAll(t.Context())
	got := readCluster(t, c)
	if v := got.Annotations[VolumeNodesAnnotation]; v != "pve1" {
		t.Errorf("records %q, want only the node in the Proxmox VE cluster", v)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, VolumesWithoutWorkerCondition)
	want := "Volume db/data-9 is on node pve9, which is not in the Proxmox VE cluster. Restore it from a backup or delete its claim."
	if cond == nil || cond.Reason != reasonNodeGone || cond.Message != want {
		t.Errorf("the condition is %+v", cond)
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
	if len(resp.Patches) != 1 || resp.Patches[0].Path != "/spec/allowedNodes" {
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
}
