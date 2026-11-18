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

	"gomodules.xyz/jsonpatch/v3"
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
	// placed are the workers as WorkerPlacement weighs them, those on their way out included.
	placed []placedWorker
}

// workersOf reads where the machines that run the cluster's pods are, those being deleted left out.
func workersOf(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (clusterWorkers, error) {
	pms, machines, err := placementMachines(ctx, c, cluster)
	if err != nil {
		return clusterWorkers{}, err
	}
	// A cluster without workers takes the taint off its control planes, which then run its pods.
	w := clusterWorkers{on: map[string][]string{}, controlPlanes: !wantsWorkers(cluster), placed: placedWorkers(pms, machines)}
	for i := range pms {
		pm := &pms[i]
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

// placementMachines lists the cluster's ProxmoxMachines and its machines, those being deleted
// included.
func placementMachines(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) ([]unstructured.Unstructured, []clusterv1.Machine, error) {
	in := []client.ListOption{client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}}
	pms := proxmoxMachineList()
	if err := c.List(ctx, pms, in...); err != nil {
		return nil, nil, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	machines := &clusterv1.MachineList{}
	if err := c.List(ctx, machines, in...); err != nil {
		return nil, nil, fmt.Errorf("listing the cluster's machines: %w", err)
	}
	return pms.Items, machines.Items, nil
}

// placedWorker is a worker as WorkerPlacement weighs it: its node, pool and MachineSet, whether it
// is deleted or marked for remediation (gone), and whether it is out of date with its pool.
type placedWorker struct {
	name, node, pool, set string
	gone, outdated        bool
}

// placedWorkers are the workers among the ProxmoxMachines, each judged by its machine.
func placedWorkers(pms []unstructured.Unstructured, machines []clusterv1.Machine) []placedWorker {
	byName := map[string]*clusterv1.Machine{}
	for i := range machines {
		byName[machines[i].Spec.InfrastructureRef.Name] = &machines[i]
	}
	var out []placedWorker
	for i := range pms {
		pm := &pms[i]
		l := pm.GetLabels()
		if l[clusterv1.MachineDeploymentNameLabel] == "" {
			continue
		}
		w := placedWorker{
			name: pm.GetName(), node: nodeOf(pm), pool: l[clusterv1.MachineDeploymentNameLabel], set: l[clusterv1.MachineSetNameLabel],
			gone: !pm.GetDeletionTimestamp().IsZero(),
		}
		if m := byName[w.name]; m != nil {
			w.gone = w.gone || !m.DeletionTimestamp.IsZero() || meta.IsStatusConditionFalse(m.Status.Conditions, clusterv1.MachineOwnerRemediatedCondition)
			w.outdated = meta.IsStatusConditionFalse(m.Status.Conditions, clusterv1.MachineUpToDateCondition)
		}
		out = append(out, w)
	}
	return out
}

// coveredNodes are the nodes where a worker other than self runs, or is about to, and stays, for a
// new worker of pool's MachineSet set: not one gone, nor one of the pool that a rollout replaces
// (another MachineSet's), nor one of another pool that is out of date.
func coveredNodes(workers []placedWorker, self, pool, set string) map[string]bool {
	covered := map[string]bool{}
	for _, w := range workers {
		samePool := w.pool == pool
		if w.name == self || w.gone || w.node == "" || samePool && w.set != set || !samePool && w.outdated {
			continue
		}
		covered[w.node] = true
	}
	return covered
}

// firstUncovered is the node a new worker goes to: the first of volumeNodes it may go to (can)
// that covered leaves out, or "" where there is none.
func firstUncovered(volumeNodes, can []string, covered map[string]bool) string {
	for _, n := range volumeNodes {
		if slices.Contains(can, n) && !covered[n] {
			return n
		}
	}
	return ""
}

// pinnable are the nodes a worker may be pinned to: the hosts, kept to chosen where the cluster
// chose any. Unlike placement, it never falls back to chosen nodes that are down.
func pinnable(chosen, hosts []string) []string {
	return slices.DeleteFunc(slices.Clone(hosts), func(n string) bool { return len(chosen) > 0 && !slices.Contains(chosen, n) })
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
	leftOut map[string]leftOut
}

// leftOut says why the Placer keeps a cluster's new VMs off an online node, as the end of a
// sentence about the node's volumes, and what lets them on. Without a fix the volumes are moved.
type leftOut struct{ why, fix string }

// cpuLeftOut is why a node whose CPU is below the level need is left out.
func cpuLeftOut(need int) leftOut {
	return leftOut{why: fmt.Sprintf("whose CPU does not reach x86-64-v%d, which the cluster's VMs need", need)}
}

// narrowedWhy adds to why the nodes of offered that preferOldest left out of hosts: those it never
// gave to fitMemory (given) for their newer major release, the others for their memory.
func narrowedWhy(online []Node, offered, given, hosts []string, why map[string]leftOut) {
	for _, n := range online {
		if !slices.Contains(offered, n.Name) || slices.Contains(hosts, n.Name) {
			continue
		}
		if !slices.Contains(given, n.Name) {
			why[n.Name] = leftOut{
				fmt.Sprintf("which runs Proxmox VE %d while nodes of an older release have room", n.Major),
				fmt.Sprintf("Upgrade the other nodes to Proxmox VE %d.", n.Major),
			}
			continue
		}
		free := "too little free memory"
		if n.MemoryKnown {
			free = NeededGiB(n.FreeMiB) + " of memory free, too little"
		}
		why[n.Name] = leftOut{"which has " + free + " for the cluster's largest machine", "Free memory on the node."}
	}
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
			round.Go(func() { p.markVolumes(ctx, &job, online, sites) })
		}
		round.Wait()
	})
}

// markVolumes records on a cluster on storage local to each node which nodes hold its volumes,
// and which of those volumes have no worker on their node. On shared storage it clears both.
func (p *Placer) markVolumes(ctx context.Context, job *volumeJob, online []Node, sites *Sites) {
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
		volumes: volumes, site: site, storage: job.storage, online: online, chosen: job.chosen, hosts: job.hosts, leftOut: job.leftOut, workers: workers,
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
// is configured, the nodes online, those the cluster chose, those a new VM of it may go to now and
// why the others are left out, and the machines that run its pods.
type volumeFacts struct {
	volumes map[string][]string
	site    StorageSite
	storage string
	online  []Node
	chosen  []string
	hosts   []string
	leftOut map[string]leftOut
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
	return nodes, withoutCondition(f, nodes, without, usable)
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
func withoutCondition(f *volumeFacts, volumeNodes []string, without []bareNode, usable int) *metav1.Condition {
	sentences := make([]string, 0, len(without))
	reason := ""
	a := newAdviser(f, volumeNodes)
	for _, b := range without {
		r := b.reason
		text := ""
		switch {
		case r == reasonNodeUnavailable:
			text = unavailableSentence(f, b.node)
		case r != "":
		case f.workers.controlPlanes:
			r, text = reasonNoWorker, fmt.Sprintf("%s on node %s, where no control plane runs. Add a worker pool, or move %s with pvecsictl migrate.",
				volumeList(f.volumes[b.node]), b.node, pronoun(f.volumes[b.node]))
		case usable > f.workers.count:
			r = reasonTooFewWorkers
		case f.workers.pending:
			continue
		default:
			r, text = reasonNoWorker, a.advise(b.node)
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

// unavailableSentence says what keeps the cluster's new VMs off node, which holds volumes.
func unavailableSentence(f *volumeFacts, node string) string {
	claims := f.volumes[node]
	lo := f.leftOut[node]
	if lo.fix == "" {
		lo.fix = "Move " + pronoun(claims) + " with pvecsictl migrate."
	}
	return fmt.Sprintf("%s on node %s, %s. %s", volumeList(claims), node, lo.why, lo.fix)
}

// adviser names, for each bare node that can take a worker, the worker whose deletion sends its
// replacement there. It asks WorkerPlacement's own rules, coveredNodes and firstUncovered, of the
// workers as they will be once the fixes before it are done.
type adviser struct {
	f           *volumeFacts
	volumeNodes []string
	workers     []placedWorker
	// settled is set while no worker is on its way out: the fixes then hold one after the other.
	settled bool
	done    int
}

func newAdviser(f *volumeFacts, volumeNodes []string) *adviser {
	workers := slices.Clone(f.workers.placed)
	settled := !slices.ContainsFunc(workers, func(w placedWorker) bool { return w.gone || w.outdated })
	return &adviser{f: f, volumeNodes: volumeNodes, workers: workers, settled: settled}
}

// advise says what gives node, which can take a worker, one.
func (a *adviser) advise(node string) string {
	lead := volumeList(a.f.volumes[node]) + " on node " + node + ", which has no worker. "
	then := func(verb string) string {
		if a.done == 0 {
			return verb
		}
		return "After that, " + strings.ToLower(verb)
	}
	if a.done == 0 || a.settled {
		for _, s := range a.spare() {
			if firstUncovered(a.volumeNodes, a.f.hosts, coveredNodes(a.workers, s.name, s.pool, s.set)) != node {
				continue
			}
			text := fmt.Sprintf("%s%s machine %s on node %s, and its replacement goes to %s.", lead, then("Delete"), s.name, s.node, node)
			a.moved(s.name, node)
			return text
		}
	}
	if a.settled {
		// Every node before this one has a worker by now, so a new worker of any pool goes here.
		a.workers = append(a.workers, placedWorker{name: "new worker on " + node, node: node})
		a.done++
		return fmt.Sprintf("%s%s a worker to a pool, and it goes to %s.", lead, then("Add"), node)
	}
	return lead + "Machine " + a.leaving() + " is on its way out. The fix shows here once it is replaced or gone."
}

// spare are the workers whose deletion uncovers no node with volumes: those on nodes without
// volumes, then all but one on each node with volumes, in the order of the nodes' names. A worker
// on its way out is none.
func (a *adviser) spare() []placedWorker {
	on := map[string][]placedWorker{}
	for _, w := range a.workers {
		if !w.gone && !w.outdated && w.node != "" {
			on[w.node] = append(on[w.node], w)
		}
	}
	var free, extra []placedWorker
	for _, n := range slices.Sorted(maps.Keys(on)) {
		workers := slices.SortedFunc(slices.Values(on[n]), func(x, y placedWorker) int { return cmp.Compare(x.name, y.name) })
		if len(a.f.volumes[n]) == 0 {
			free = append(free, workers...)
			continue
		}
		extra = append(extra, workers[1:]...)
	}
	return append(free, extra...)
}

// moved has the worker named replaced by one on node.
func (a *adviser) moved(name, node string) {
	i := slices.IndexFunc(a.workers, func(w placedWorker) bool { return w.name == name })
	a.workers[i].node = node
	a.done++
}

// leaving names the first worker on its way out, of which an unsettled cluster has one at least.
func (a *adviser) leaving() string {
	var names []string
	for _, w := range a.workers {
		if w.gone || w.outdated {
			names = append(names, w.name)
		}
	}
	return slices.Min(names)
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
	// The cluster may be deleted while its volumes are read.
	if err := p.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "recording the nodes that hold volumes", "cluster", client.ObjectKeyFromObject(c))
	}
}

// unpinStale takes the pin off each of the cluster's machines that WorkerPlacement kept to a node a
// new VM of the cluster can no longer go to, before its VM is made. capmox then places it on the
// nodes the cluster names, where it would otherwise wait for the health check to replace it. A pin
// outlives the volume record, so the machines are read whatever it holds.
func (p *Placer) unpinStale(ctx context.Context, c *clusterv1.Cluster, can []string) {
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
	// Only nodes the record names as taking VMs now, so online and out of maintenance.
	can := pinnable(chosen, online)
	if own, _, _ := unstructured.NestedStringSlice(pm.Object, "spec", "allowedNodes"); len(own) > 0 {
		can = slices.DeleteFunc(can, func(n string) bool { return !slices.Contains(own, n) })
	}
	pms, machines, err := placementMachines(ctx, w.Reader, c)
	if err != nil {
		return "", err
	}
	return firstUncovered(volumeNodes, can, coveredNodes(placedWorkers(pms, machines), pm.GetName(), pool, set)), nil
}

// byVolumes orders the nodes with volumes by how many they hold, most first, then by name.
func byVolumes(volumes map[string][]string, nodes []string) []string {
	return slices.SortedStableFunc(slices.Values(nodes), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(volumes[b]), len(volumes[a])), cmp.Compare(a, b))
	})
}
