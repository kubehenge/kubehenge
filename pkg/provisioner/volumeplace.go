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
	"sync"
	"time"

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
// nodes its bound volumes are on, the one with the most volumes first. A pod can use such a
// volume only on a node of the cluster on the volume's node, so WorkerPlacement sends a new worker
// to one of them that is left without one.
const VolumeNodesAnnotation = "kubehenge.dev/volume-nodes"

// VolumePinAnnotation is on a ProxmoxMachine that WorkerPlacement kept to the node it names. The
// Placer takes the pin off again when that node can no longer take the VM before it is made.
const VolumePinAnnotation = "kubehenge.dev/volume-pin"

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
	reasonStorageInactive = "StorageInactive"
	reasonNodeUnavailable = "NodeUnavailable"
	reasonTooFewWorkers   = "TooFewWorkers"
	reasonNoWorker        = "NoWorker"
)

var reasonOrder = []string{
	reasonNodeGone, reasonNodeNotAllowed, reasonNodeOffline, reasonNodeMaintenance, reasonStorageInactive,
	reasonNodeUnavailable, reasonTooFewWorkers, reasonNoWorker,
}

// volumesNamed is how many volumes a sentence of the condition names before it counts the rest.
const volumesNamed = 3

// handleFields are those of the CSI plugin's volume handle: region/zone/storage/volume.
const handleFields = 4

// defaultVolumeTimeout bounds the read of one cluster's volumes where Placer.VolumeTimeout is zero.
// A cluster that answers takes well under a second, and one that does not is tried again next round.
const defaultVolumeTimeout = 5 * time.Second

// placerFieldOwner writes what the Placer changes on ProxmoxMachines.
const placerFieldOwner = client.FieldOwner("kubehenge-placer")

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

// clusterWorkers are the machines that run a cluster's pods: its workers, or its control planes
// where it has no workers to ask for.
type clusterWorkers struct {
	// on names the machines on each node.
	on    map[string][]string
	count int
	// pending is set while one of them is still being placed.
	pending bool
	// controlPlanes is set where the control planes run the pods.
	controlPlanes bool
}

// workersOf reads where the machines that run the cluster's pods are, those being deleted left out.
func workersOf(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (clusterWorkers, error) {
	pms := proxmoxMachineList()
	if err := c.List(ctx, pms, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return clusterWorkers{}, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	// A cluster without workers takes the taint off its control planes, which then run its pods.
	w := clusterWorkers{on: map[string][]string{}, controlPlanes: !wantsWorkers(cluster)}
	for i := range pms.Items {
		pm := &pms.Items[i]
		_, controlPlane := pm.GetLabels()[clusterv1.MachineControlPlaneLabel]
		worker := pm.GetLabels()[clusterv1.MachineDeploymentNameLabel] != ""
		runsPods := worker || w.controlPlanes && controlPlane
		if !runsPods || !pm.GetDeletionTimestamp().IsZero() {
			continue
		}
		w.count++
		if node := nodeOf(pm); node != "" {
			w.on[node] = append(w.on[node], pm.GetName())
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

// volumeJob is what reading one cluster's volumes needs from its placement: the nodes it chose,
// its storage, and the nodes a new VM of it may go to now.
type volumeJob struct {
	cluster *clusterv1.Cluster
	chosen  []string
	storage string
	hosts   []string
}

// startVolumes reads the clusters' volumes beside the placement, every cluster at once, so that
// neither the placement nor another cluster waits on one that is slow to answer. A round still
// running when the next placement ends is left to finish, and that placement's round is skipped.
func (p *Placer) startVolumes(ctx context.Context, jobs []volumeJob, online []Node, sites *Sites) {
	if p.Workload == nil || sites == nil || len(jobs) == 0 || !p.volumesBusy.CompareAndSwap(false, true) {
		return
	}
	p.volumes.Go(func() {
		defer p.volumesBusy.Store(false)
		var round sync.WaitGroup
		for _, job := range jobs {
			round.Go(func() { p.markVolumes(ctx, job, online, sites) })
		}
		round.Wait()
	})
}

// markVolumes records on a cluster on storage local to each node which nodes hold its volumes,
// and which of those volumes have no worker on their node. On shared storage it clears both.
func (p *Placer) markVolumes(ctx context.Context, job volumeJob, online []Node, sites *Sites) {
	c := job.cluster
	log := ctrl.LoggerFrom(ctx).WithName("placer").WithValues("cluster", client.ObjectKeyFromObject(c))
	site, known := sites.Storages[job.storage]
	var volumes map[string][]string
	if known && !site.Shared {
		// Until its control plane is up a cluster has no volumes, and its API does not answer.
		if !meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
			return
		}
		var read bool
		if volumes, read = p.readVolumes(ctx, c, job.storage); !read {
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
	nodes, cond := volumeReport(&volumeFacts{
		volumes: volumes, site: site, storage: job.storage, online: online, chosen: job.chosen, hosts: job.hosts, workers: workers,
	})
	p.recordVolumeNodes(ctx, c, nodes)
	have := meta.FindStatusCondition(c.Status.Conditions, VolumesWithoutWorkerCondition)
	if (cond == nil && have == nil) || (cond != nil && have != nil && have.Reason == cond.Reason && have.Message == cond.Message) {
		return
	}
	if _, err := markCluster(ctx, p.Client, p.Client, client.ObjectKeyFromObject(c), VolumesWithoutWorkerCondition, cond); err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		log.Error(err, "recording the volumes without a worker")
	}
}

// readVolumes reads the cluster's bound volumes on storage by node, within the Placer's
// VolumeTimeout. read is false while the cluster cannot be read in that time. A read that runs
// over is left to end on the client's own timeout.
func (p *Placer) readVolumes(ctx context.Context, c *clusterv1.Cluster, storage string) (map[string][]string, bool) {
	log := ctrl.LoggerFrom(ctx).WithName("placer").WithValues("cluster", client.ObjectKeyFromObject(c))
	timeout := cmp.Or(p.VolumeTimeout, defaultVolumeTimeout)
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		pvs   []corev1.PersistentVolume
		found bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reader, found, err := p.Workload(bounded, c)
		if err != nil || !found {
			done <- result{found: found, err: err}
			return
		}
		pvs := &corev1.PersistentVolumeList{}
		err = reader.List(bounded, pvs)
		done <- result{pvs: pvs.Items, found: true, err: err}
	}()
	select {
	case r := <-done:
		if r.err != nil && ctx.Err() == nil {
			log.Error(r.err, "reading the cluster's persistent volumes")
		}
		if r.err != nil || !r.found {
			return nil, false
		}
		return volumesByNode(r.pvs, storage), true
	case <-bounded.Done():
		if ctx.Err() == nil {
			log.Info("the cluster's persistent volumes were not read in time", "timeout", timeout.String())
		}
		return nil, false
	}
}

// volumeFacts are what volumeReport judges: the volumes by node, the cluster's storage and where it
// is configured, the nodes online, those the cluster chose and those a new VM of it may go to now,
// and the machines that run its pods.
type volumeFacts struct {
	volumes map[string][]string
	site    StorageSite
	storage string
	online  []Node
	chosen  []string
	hosts   []string
	workers clusterWorkers
}

// bareNode is a node holding volumes without a worker, and why where it cannot take one.
type bareNode struct{ node, reason string }

// volumeReport is which of the nodes holding volumes are in the Proxmox VE cluster, the one with
// the most volumes first, and the condition naming the volumes whose node has no worker, nil when
// every one has.
func volumeReport(f *volumeFacts) ([]string, *metav1.Condition) {
	var nodes []string
	var without []bareNode
	// usable counts the nodes with volumes that a new VM of the cluster may go to now.
	usable := 0
	// The webhook fills the nodes in this order, so the advice follows it too.
	for _, n := range byVolumes(f.volumes, slices.Collect(maps.Keys(f.volumes))) {
		if !slices.Contains(f.site.Nodes, n) {
			without = append(without, bareNode{n, reasonNodeGone})
			continue
		}
		nodes = append(nodes, n)
		if slices.Contains(f.hosts, n) {
			usable++
		}
		if len(f.workers.on[n]) > 0 {
			continue
		}
		without = append(without, bareNode{n, whyNoWorker(f, n)})
	}
	return nodes, withoutCondition(f, without, usable)
}

// whyNoWorker is why a node with volumes and without a worker cannot take one, "" where it can.
func whyNoWorker(f *volumeFacts, n string) string {
	i := slices.IndexFunc(f.online, func(o Node) bool { return o.Name == n })
	switch {
	case len(f.chosen) > 0 && !slices.Contains(f.chosen, n):
		return reasonNodeNotAllowed
	case i < 0:
		return reasonNodeOffline
	case f.online[i].Maintenance:
		return reasonNodeMaintenance
	case slices.Contains(f.online[i].InactiveStorages, f.storage) || !slices.Contains(f.site.Active, n):
		return reasonStorageInactive
	case !slices.Contains(f.hosts, n):
		return reasonNodeUnavailable
	}
	return ""
}

// withoutCondition says why each bare node has no worker and what gives it one, nil when none is
// left to say: a node that can take a worker needs nothing said while one is on its way, as that
// one is sent there first.
func withoutCondition(f *volumeFacts, without []bareNode, usable int) *metav1.Condition {
	sentences := make([]string, 0, len(without))
	reason := ""
	spare := spareWorkers(f)
	next := 0
	for _, b := range without {
		r := b.reason
		text := ""
		if r == "" {
			switch {
			case f.workers.controlPlanes:
				r, text = reasonNoWorker, fmt.Sprintf("%s on node %s, where no control plane runs. Add a worker pool, or move %s with pvecsictl migrate.",
					volumeList(f.volumes[b.node]), b.node, pronoun(f.volumes[b.node]))
			case usable > f.workers.count:
				r = reasonTooFewWorkers
			case f.workers.pending:
				continue
			default:
				r, text = reasonNoWorker, noWorkerSentence(f, b.node, spare, next)
				next++
			}
		}
		if text == "" {
			text = volumeSentence(r, b.node, f.volumes[b.node], f.storage, usable, f.workers.count)
		}
		sentences = append(sentences, text)
		if reason == "" || slices.Index(reasonOrder, r) < slices.Index(reasonOrder, reason) {
			reason = r
		}
	}
	if len(sentences) == 0 {
		return nil
	}
	return &metav1.Condition{Status: metav1.ConditionTrue, Reason: reason, Message: strings.Join(sentences, " ")}
}

// spareWorker is a worker whose deletion leaves no node with volumes without one.
type spareWorker struct{ name, node string }

// spareWorkers are the workers whose deletion uncovers no node with volumes: those on nodes without
// volumes, then all but one on each node with volumes, in the order of the nodes' names.
func spareWorkers(f *volumeFacts) []spareWorker {
	var free, extra []spareWorker
	for _, n := range slices.Sorted(maps.Keys(f.workers.on)) {
		names := slices.Sorted(slices.Values(f.workers.on[n]))
		if len(f.volumes[n]) == 0 {
			for _, name := range names {
				free = append(free, spareWorker{name, n})
			}
			continue
		}
		for _, name := range names[1:] {
			extra = append(extra, spareWorker{name, n})
		}
	}
	return append(free, extra...)
}

// noWorkerSentence gives the next bare node that can take a worker the worker to delete for it.
// The replacement of a deleted worker goes to the first node with volumes and no worker, so each
// fix holds once those before it are done.
func noWorkerSentence(f *volumeFacts, node string, spare []spareWorker, next int) string {
	what := volumeList(f.volumes[node])
	then := ""
	if next > 0 {
		then = "After that, "
	}
	if next < len(spare) {
		s := spare[next]
		verb := "Delete"
		if then != "" {
			verb = "delete"
		}
		return fmt.Sprintf("%s on node %s, which has no worker. %s%s machine %s on node %s, and its replacement goes to %s.",
			what, node, then, verb, s.name, s.node, node)
	}
	verb := "Add"
	if then != "" {
		verb = "add"
	}
	return fmt.Sprintf("%s on node %s, which has no worker. %s%s a worker to a pool, and it goes to %s.", what, node, then, verb, node)
}

func pronoun(claims []string) string {
	if len(claims) == 1 {
		return "it"
	}
	return "them"
}

// volumeSentence says why the volumes on node have no worker there and what gives them one.
func volumeSentence(reason, node string, claims []string, storage string, usable, workers int) string {
	what := volumeList(claims)
	them, claimsOf := pronoun(claims), "their claims"
	if len(claims) == 1 {
		claimsOf = "its claim"
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
	case reasonStorageInactive:
		return fmt.Sprintf("%s on node %s, where storage %s is not active. Activate %s on %s.", what, node, storage, storage, node)
	case reasonNodeUnavailable:
		return fmt.Sprintf("%s on node %s, which cannot take this cluster's VMs now. The cluster's other conditions say why.", what, node)
	default:
		return fmt.Sprintf("%s on node %s, which has no worker. Volumes are on %d nodes and the cluster has %d workers, so add workers to a pool.", what, node, usable, workers)
	}
}

// volumeList names the volumes, the first few by name, as the subject of a sentence.
func volumeList(claims []string) string {
	sorted := slices.Sorted(slices.Values(claims))
	noun, verb := "Volumes", "are"
	if len(sorted) == 1 {
		noun, verb = "Volume", "is"
	}
	named := sorted[:min(len(sorted), volumesNamed)]
	text := strings.Join(named, ", ")
	if rest := len(sorted) - len(named); rest > 0 {
		text += fmt.Sprintf(" and %d more", rest)
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

// unpinStale takes the pin off each of the cluster's machines that WorkerPlacement kept to a node a
// new VM of the cluster can no longer go to, before its VM is made. capmox then places it on the
// nodes the cluster names, where it would otherwise wait for the health check to replace it.
func (p *Placer) unpinStale(ctx context.Context, c *clusterv1.Cluster, can []string) {
	if c.Annotations[VolumeNodesAnnotation] == "" {
		return
	}
	log := ctrl.LoggerFrom(ctx).WithName("placer").WithValues("cluster", client.ObjectKeyFromObject(c))
	pms := proxmoxMachineList()
	if err := p.Client.List(ctx, pms, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		if ctx.Err() == nil {
			log.Error(err, "listing the cluster's ProxmoxMachines")
		}
		return
	}
	for i := range pms.Items {
		pm := &pms.Items[i]
		pin := pm.GetAnnotations()[VolumePinAnnotation]
		if pin == "" || slices.Contains(can, pin) || !pm.GetDeletionTimestamp().IsZero() || made(pm) {
			continue
		}
		patch := []byte(`{"metadata":{"annotations":{"` + VolumePinAnnotation + `":null}},"spec":{"allowedNodes":null}}`)
		if err := p.Client.Patch(ctx, pm, client.RawPatch(types.MergePatchType, patch), placerFieldOwner); err != nil && ctx.Err() == nil {
			log.Error(err, "taking the pin off a machine kept to a node it cannot go to", "proxmoxMachine", pm.GetName(), "node", pin)
		}
	}
}

// made tells whether capmox has started on a ProxmoxMachine's VM.
func made(pm *unstructured.Unstructured) bool {
	node, _, _ := unstructured.NestedString(pm.Object, "status", "proxmoxNode")
	vmid, _, _ := unstructured.NestedInt64(pm.Object, "spec", "virtualMachineID")
	return node != "" || vmid != 0
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
	pin := jsonpatch.NewOperation(jsonPatchAdd, "/metadata/annotations", map[string]string{VolumePinAnnotation: node})
	if pm.GetAnnotations() != nil {
		pin = jsonpatch.NewOperation(jsonPatchAdd, "/metadata/annotations/"+strings.ReplaceAll(VolumePinAnnotation, "/", "~1"), node)
	}
	return admission.Patched("", jsonpatch.NewOperation(jsonPatchAdd, "/spec/allowedNodes", []string{node}), pin)
}

// nodeFor is the node with volumes and no other worker that a new worker goes to, the one with the
// most volumes first, or "" where every such node the cluster may use now has a worker.
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
	// Only nodes the record names as taking VMs now, so online and out of maintenance: placement()
	// keeps a cluster to its chosen nodes while all are down, which a pin must not.
	can := slices.DeleteFunc(slices.Clone(online), func(n string) bool { return len(chosen) > 0 && !slices.Contains(chosen, n) })
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
