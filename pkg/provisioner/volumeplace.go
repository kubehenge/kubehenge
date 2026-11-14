package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"gomodules.xyz/jsonpatch/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// VolumeNodesAnnotation lists, on a cluster whose storage is local to each node, the Proxmox VE
// nodes its bound volumes are on. A pod can use such a volume only on a worker on its node, so
// WorkerPlacement sends a new worker to one of them that is left without one.
const VolumeNodesAnnotation = "kubehenge.dev/volume-nodes"

// VolumesWithoutWorkerCondition on a Cluster names the bound volumes whose node has no worker,
// whose pods therefore stay Pending, and what gives them one.
const VolumesWithoutWorkerCondition = "VolumesWithoutWorker"

// PlaceWorkerPath is the admission webhook that sends a new worker to a node with volumes.
const PlaceWorkerPath = "/place-worker"

// Why a volume's node has no worker, in the order the condition's reason takes them.
const (
	reasonNodeGone        = "NodeGone"
	reasonNodeNotAllowed  = "NodeNotAllowed"
	reasonNodeOffline     = "NodeOffline"
	reasonNodeMaintenance = "NodeInMaintenance"
	reasonTooFewWorkers   = "TooFewWorkers"
	reasonNoWorker        = "NoWorker"
)

// volumesNamed is how many volumes a sentence of the condition names before it counts the rest.
const volumesNamed = 3

// handleFields are those of the CSI plugin's volume handle: region/zone/storage/volume.
const handleFields = 4

// volumesByNode are the bound volumes of the CSI plugin on storage, each as its claim's
// namespace/name, by the Proxmox VE node their node affinity keeps them to. A volume that more
// than one node reaches is left out.
func volumesByNode(pvs []corev1.PersistentVolume, storage string) map[string][]string {
	out := map[string][]string{}
	for i := range pvs {
		pv := &pvs[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csiDriver || pv.Status.Phase != corev1.VolumeBound {
			continue
		}
		if parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, "/", handleFields); len(parts) != handleFields || parts[2] != storage {
			continue
		}
		if node := zoneOf(pv); node != "" {
			out[node] = append(out[node], claimOf(pv))
		}
	}
	return out
}

// zoneOf is the one zone, the Proxmox VE node, a volume's node affinity requires, if it requires one.
func zoneOf(pv *corev1.PersistentVolume) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	zone := ""
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key != corev1.LabelTopologyZone || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 {
				continue
			}
			if zone != "" && zone != e.Values[0] {
				return ""
			}
			zone = e.Values[0]
		}
	}
	return zone
}

// claimOf names a volume as kubectl users know it: by its claim, or by itself without one.
func claimOf(pv *corev1.PersistentVolume) string {
	if r := pv.Spec.ClaimRef; r != nil && r.Name != "" {
		return r.Namespace + "/" + r.Name
	}
	return pv.Name
}

// clusterWorkers are where a cluster's workers run, how many there are, and whether one is still
// being placed.
type clusterWorkers struct {
	on      map[string]int
	count   int
	pending bool
}

// workersOf reads where the cluster's workers that are not being deleted run.
func workersOf(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (clusterWorkers, error) {
	pms := proxmoxMachineList()
	if err := c.List(ctx, pms, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return clusterWorkers{}, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	w := clusterWorkers{on: map[string]int{}}
	for i := range pms.Items {
		pm := &pms.Items[i]
		if pm.GetLabels()[clusterv1.MachineDeploymentNameLabel] == "" || !pm.GetDeletionTimestamp().IsZero() {
			continue
		}
		w.count++
		if node := nodeOf(pm); node != "" {
			w.on[node]++
		} else {
			w.pending = true
		}
	}
	return w, nil
}

// nodeOf is the node a ProxmoxMachine's VM is on, or the one node it may go to before it is cloned.
func nodeOf(pm *unstructured.Unstructured) string {
	if node, _, _ := unstructured.NestedString(pm.Object, "status", "proxmoxNode"); node != "" {
		return node
	}
	if allowed, _, _ := unstructured.NestedStringSlice(pm.Object, "spec", "allowedNodes"); len(allowed) == 1 {
		return allowed[0]
	}
	return ""
}

// markVolumes records on a cluster on storage local to each node which nodes hold its volumes,
// and which of those volumes have no worker on their node. On shared storage it clears both.
func (p *Placer) markVolumes(ctx context.Context, c *clusterv1.Cluster, online []Node, sites *Sites, chosen []string, storage string) {
	if p.Workload == nil || sites == nil {
		return
	}
	log := ctrl.LoggerFrom(ctx).WithName("placer").WithValues("cluster", client.ObjectKeyFromObject(c))
	site, known := sites.Storages[storage]
	var volumes map[string][]string
	if known && !site.Shared {
		var read bool
		if volumes, read = p.readVolumes(ctx, c, storage); !read {
			return
		}
	}
	var workers clusterWorkers
	if len(volumes) > 0 {
		var err error
		if workers, err = workersOf(ctx, p.Client, c); err != nil {
			log.Error(err, "reading where the cluster's workers run")
			return
		}
	}
	nodes, cond := volumeReport(volumes, site.Nodes, online, chosen, workers)
	p.recordVolumeNodes(ctx, c, nodes)
	have := meta.FindStatusCondition(c.Status.Conditions, VolumesWithoutWorkerCondition)
	if (cond == nil && have == nil) || (cond != nil && have != nil && have.Reason == cond.Reason && have.Message == cond.Message) {
		return
	}
	if _, err := markCluster(ctx, p.Client, p.Client, client.ObjectKeyFromObject(c), VolumesWithoutWorkerCondition, cond); err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		log.Error(err, "recording the volumes without a worker")
	}
}

// readVolumes reads the cluster's bound volumes on storage by node, and read is false while the
// cluster cannot be read.
func (p *Placer) readVolumes(ctx context.Context, c *clusterv1.Cluster, storage string) (map[string][]string, bool) {
	log := ctrl.LoggerFrom(ctx).WithName("placer").WithValues("cluster", client.ObjectKeyFromObject(c))
	reader, found, err := p.Workload(ctx, c)
	if err != nil || !found {
		if err != nil && ctx.Err() == nil {
			log.Error(err, "reaching the cluster to read its volumes")
		}
		return nil, false
	}
	pvs := &corev1.PersistentVolumeList{}
	if err := reader.List(ctx, pvs); err != nil {
		if ctx.Err() == nil {
			log.Error(err, "listing the cluster's persistent volumes")
		}
		return nil, false
	}
	return volumesByNode(pvs.Items, storage), true
}

// bareNode is a node holding volumes without a worker, and why where it cannot take one.
type bareNode struct{ node, reason string }

// volumeReport is which of the nodes holding volumes are in the Proxmox VE cluster (existing), the
// one with the most volumes first, and the condition naming the volumes whose node has no worker,
// nil when every one has.
func volumeReport(volumes map[string][]string, existing []string, online []Node, chosen []string, workers clusterWorkers) ([]string, *metav1.Condition) {
	var nodes []string
	type bare = bareNode
	var without []bare
	// usable counts the nodes with volumes that can take a worker.
	usable := 0
	for _, n := range slices.Sorted(maps.Keys(volumes)) {
		if !slices.Contains(existing, n) {
			without = append(without, bare{n, reasonNodeGone})
			continue
		}
		nodes = append(nodes, n)
		i := slices.IndexFunc(online, func(o Node) bool { return o.Name == n })
		allowed := len(chosen) == 0 || slices.Contains(chosen, n)
		if allowed && i >= 0 && !online[i].Maintenance {
			usable++
		}
		if workers.on[n] > 0 {
			continue
		}
		switch {
		case !allowed:
			without = append(without, bare{n, reasonNodeNotAllowed})
		case i < 0:
			without = append(without, bare{n, reasonNodeOffline})
		case online[i].Maintenance:
			without = append(without, bare{n, reasonNodeMaintenance})
		default:
			without = append(without, bare{n, ""})
		}
	}
	return byVolumes(volumes, nodes), withoutCondition(without, volumes, usable, workers)
}

// withoutCondition says why each bare node has no worker, nil when none is left to say: a node
// that can take a worker needs none said while one is on its way, as that one is sent there first.
func withoutCondition(without []bareNode, volumes map[string][]string, usable int, workers clusterWorkers) *metav1.Condition {
	sentences := make([]string, 0, len(without))
	reason := ""
	for _, b := range without {
		r := b.reason
		if r == "" {
			switch {
			case usable > workers.count:
				r = reasonTooFewWorkers
			case workers.pending:
				continue
			default:
				r = reasonNoWorker
			}
		}
		sentences = append(sentences, volumeSentence(r, b.node, volumes[b.node], usable, workers.count))
		if reason == "" || reasonRank(r) < reasonRank(reason) {
			reason = r
		}
	}
	if len(sentences) == 0 {
		return nil
	}
	return &metav1.Condition{Status: metav1.ConditionTrue, Reason: reason, Message: strings.Join(sentences, " ")}
}

func reasonRank(reason string) int {
	return slices.Index([]string{reasonNodeGone, reasonNodeNotAllowed, reasonNodeOffline, reasonNodeMaintenance, reasonTooFewWorkers, reasonNoWorker}, reason)
}

// volumeSentence says why the volumes on node have no worker there and what gives them one.
func volumeSentence(reason, node string, claims []string, usable, workers int) string {
	what := volumeList(claims)
	them, claimsOf := "them", "their claims"
	if len(claims) == 1 {
		them, claimsOf = "it", "its claim"
	}
	switch reason {
	case reasonNodeGone:
		return fmt.Sprintf("%s on node %s, which is not in the Proxmox VE cluster. Restore %s from a backup or delete %s.", what, node, them, claimsOf)
	case reasonNodeNotAllowed:
		return fmt.Sprintf("%s on node %s, which this cluster may not use. Add %s to allowedNodes, or move %s with pvecsictl migrate.", what, node, node, them)
	case reasonNodeOffline:
		return fmt.Sprintf("%s on node %s, which is offline. Bring %s back online.", what, node, node)
	case reasonNodeMaintenance:
		return fmt.Sprintf("%s on node %s, which is in maintenance. Their pods start once %s leaves maintenance.", what, node, node)
	case reasonTooFewWorkers:
		return fmt.Sprintf("%s on node %s, which has no worker. Volumes are on %d nodes and the cluster has %d workers, so add workers to a pool.", what, node, usable, workers)
	default:
		return fmt.Sprintf("%s on node %s, which has no worker. Delete a worker on another node and its replacement goes to %s.", what, node, node)
	}
}

// volumeList names the volumes, the first few by name, as the subject of a sentence.
func volumeList(claims []string) string {
	sorted := slices.Sorted(slices.Values(claims))
	noun := "Volumes"
	if len(sorted) == 1 {
		noun = "Volume"
	}
	named := sorted[:min(len(sorted), volumesNamed)]
	text := strings.Join(named, ", ")
	if rest := len(sorted) - len(named); rest > 0 {
		text += fmt.Sprintf(" and %d more", rest)
	}
	verb := "are"
	if len(sorted) == 1 {
		verb = "is"
	}
	return noun + " " + text + " " + verb
}

// recordVolumeNodes writes the nodes that hold the cluster's volumes, when they changed.
func (p *Placer) recordVolumeNodes(ctx context.Context, c *clusterv1.Cluster, nodes []string) {
	value := strings.Join(nodes, ",")
	current, has := c.Annotations[VolumeNodesAnnotation]
	if current == value && (has || value == "") {
		return
	}
	var set *string
	if value != "" {
		set = &value
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: map[string]any{VolumeNodesAnnotation: set}}})
	if err != nil {
		return
	}
	target := &clusterv1.Cluster{}
	target.Namespace, target.Name = c.Namespace, c.Name
	if err := p.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "recording the nodes that hold volumes", "cluster", client.ObjectKeyFromObject(c))
	}
}

// WorkerPlacement sends a new worker of a cluster with volumes on storage local to each node to a
// node that holds volumes and keeps no other worker: in a rollout to its predecessor's node, as
// a pool grows to the nodes with volumes first. capmox clones a ProxmoxMachine onto a node its own
// spec.allowedNodes names, and the cluster-wide list goes on the ProxmoxCluster that every machine
// shares. A pool's machine template is no place for it either, as each change there replaces the
// pool's nodes. Elsewhere it lets the machine through as it is.
type WorkerPlacement struct {
	Reader client.Reader
}

var _ admission.Handler = &WorkerPlacement{}

// Handle implements admission.Handler.
func (w *WorkerPlacement) Handle(ctx context.Context, req admission.Request) admission.Response { //nolint:gocritic // admission.Handler's signature takes the request by value
	pm := &unstructured.Unstructured{}
	if err := pm.UnmarshalJSON(req.Object.Raw); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	node, err := w.nodeFor(ctx, pm, req.Namespace)
	if err != nil {
		// Placed where capmox would place it, the machine still comes up.
		ctrl.LoggerFrom(ctx).Error(err, "choosing a node with volumes for a new worker", "proxmoxMachine", pm.GetName())
		return admission.Allowed("")
	}
	if node == "" {
		return admission.Allowed("")
	}
	return admission.Patched("", jsonpatch.NewOperation("add", "/spec/allowedNodes", []string{node}))
}

// nodeFor is the node with volumes and no other worker that a new worker goes to, the one with the
// most volumes first, or "" where every such node the cluster may use has a worker.
func (w *WorkerPlacement) nodeFor(ctx context.Context, pm *unstructured.Unstructured, namespace string) (string, error) {
	labels := pm.GetLabels()
	pool, set, name := labels[clusterv1.MachineDeploymentNameLabel], labels[clusterv1.MachineSetNameLabel], labels[clusterv1.ClusterNameLabel]
	if pool == "" || name == "" {
		return "", nil
	}
	if namespace == "" {
		namespace = pm.GetNamespace()
	}
	c := &clusterv1.Cluster{}
	if err := w.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, c); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading cluster %s/%s: %w", namespace, name, err)
	}
	recorded, online := c.Annotations[VolumeNodesAnnotation], splitList(c.Annotations[OnlineNodesAnnotation])
	if recorded == "" || len(online) == 0 {
		return "", nil
	}
	// In the annotation's order, the node with the most volumes first.
	volumeNodes := strings.Split(recorded, ",")
	chosen, _, err := optionalVariable[[]string](c, allowedNodesVariable)
	if err != nil {
		return "", err
	}
	can := placement(chosen, online)
	if own, _, _ := unstructured.NestedStringSlice(pm.Object, "spec", "allowedNodes"); len(own) > 0 {
		can = slices.DeleteFunc(can, func(n string) bool { return !slices.Contains(own, n) })
	}
	covered, err := w.covered(ctx, c, pm.GetName(), pool, set)
	if err != nil {
		return "", err
	}
	for _, n := range volumeNodes {
		if slices.Contains(can, n) && !covered[n] {
			return n, nil
		}
	}
	return "", nil
}

// covered are the nodes where a worker other than the new one runs, or is about to, and stays:
// not one being deleted or remediated, nor one of the new worker's pool that a rollout replaces
// (another MachineSet's), nor one of another pool that is out of date.
func (w *WorkerPlacement) covered(ctx context.Context, c *clusterv1.Cluster, self, pool, set string) (map[string]bool, error) {
	in := []client.ListOption{client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}}
	pms := proxmoxMachineList()
	if err := w.Reader.List(ctx, pms, in...); err != nil {
		return nil, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	machines := &clusterv1.MachineList{}
	if err := w.Reader.List(ctx, machines, in...); err != nil {
		return nil, fmt.Errorf("listing the cluster's machines: %w", err)
	}
	byName := map[string]*clusterv1.Machine{}
	for i := range machines.Items {
		byName[machines.Items[i].Spec.InfrastructureRef.Name] = &machines.Items[i]
	}
	covered := map[string]bool{}
	for i := range pms.Items {
		pm := &pms.Items[i]
		l := pm.GetLabels()
		if pm.GetName() == self || l[clusterv1.MachineDeploymentNameLabel] == "" || !pm.GetDeletionTimestamp().IsZero() {
			continue
		}
		samePool := l[clusterv1.MachineDeploymentNameLabel] == pool
		if samePool && l[clusterv1.MachineSetNameLabel] != set {
			continue
		}
		if m := byName[pm.GetName()]; m != nil && leaving(m, !samePool) {
			continue
		}
		if node := nodeOf(pm); node != "" {
			covered[node] = true
		}
	}
	return covered, nil
}

// leaving tells whether a machine is on its way out: deleted, marked for remediation, or, where
// outdated counts, no longer up to date with its pool.
func leaving(m *clusterv1.Machine, outdated bool) bool {
	if !m.DeletionTimestamp.IsZero() {
		return true
	}
	if meta.IsStatusConditionFalse(m.Status.Conditions, clusterv1.MachineOwnerRemediatedCondition) {
		return true
	}
	return outdated && meta.IsStatusConditionFalse(m.Status.Conditions, clusterv1.MachineUpToDateCondition)
}

// byVolumes orders the nodes with volumes by how many they hold, most first, then by name.
func byVolumes(volumes map[string][]string, nodes []string) []string {
	return slices.SortedStableFunc(slices.Values(nodes), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(volumes[b]), len(volumes[a])), cmp.Compare(a, b))
	})
}
