package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OnlineNodesAnnotation lists, on each Cluster, the Proxmox VE nodes last seen online whose CPU runs
// the cluster's node OS, or Talos. Changing it has Cluster API compute the cluster's topology again, and with
// it the nodes capmox may use.
const OnlineNodesAnnotation = "kubehenge.dev/online-nodes"

// PlacementBlockedAnnotation is on a Cluster while every Proxmox VE node it could use is in
// maintenance or has the cluster's storage inactive. It says why, and Cluster API's patch of the
// cluster is refused with that text until a node can take a VM.
const PlacementBlockedAnnotation = "kubehenge.dev/placement-blocked"

// allowedNodesVariable is the ClusterClass variable naming the nodes a cluster's VMs keep to.
const allowedNodesVariable = "allowedNodes"

// storageVariable is the ClusterClass variable naming the storage a cluster's disks are on.
const storageVariable = "storage"

// placementVariables are the nodes a cluster keeps to, if it chose any, and its storage.
func placementVariables(c *clusterv1.Cluster) ([]string, string, error) {
	chosen, _, err := optionalVariable[[]string](c, allowedNodesVariable)
	if err != nil {
		return nil, "", err
	}
	storage, _, err := optionalVariable[string](c, storageVariable)
	return chosen, storage, err
}

// Placer keeps OnlineNodesAnnotation current on every Kubehenge cluster.
type Placer struct {
	Client         client.Client
	ClassNamespace string
	Nodes          Nodes
	// Sites, when set, leaves out of a cluster's placement the nodes its storage is not available
	// on and the nodes its templates cannot be cloned on.
	Sites SiteSource
	// Interval is how often the nodes are looked at: a node that goes down is left out of the
	// clusters' placement within about that long.
	Interval time.Duration
}

// NodesSkippedCondition on a Cluster says that Placer left Proxmox VE nodes out of its placement
// because their status could not be read.
const NodesSkippedCondition = "NodesSkipped"

// skippingNodes is implemented by a Nodes that skips the nodes it cannot read.
type skippingNodes interface {
	Unreadable() []string
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*Placer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (p *Placer) Start(ctx context.Context) error {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.placeAll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (p *Placer) placeAll(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx).WithName("placer")
	online, err := p.Nodes.OnlineNodes(ctx)
	if err != nil {
		log.Error(err, "reading the Proxmox VE nodes")
		return
	}
	var sites *Sites
	if p.Sites != nil {
		if sites, err = p.Sites.Sites(ctx); err != nil {
			log.Error(err, "reading the Proxmox VE storages and templates")
			return
		}
	}
	var skipped []string
	if u, ok := p.Nodes.(skippingNodes); ok {
		skipped = u.Unreadable()
	}
	clusters := &clusterv1.ClusterList{}
	if err := p.Client.List(ctx, clusters, client.UnsafeDisableDeepCopy); err != nil {
		log.Error(err, "listing clusters")
		return
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !Owns(c, p.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		p.markSkipped(ctx, c, skipped)
		level, err := clusterCPULevel(c)
		if err != nil {
			log.Error(err, "reading the cluster's node OS", "cluster", client.ObjectKeyFromObject(c))
			continue
		}
		chosen, storage, err := placementVariables(c)
		if err != nil {
			log.Error(err, "reading the cluster's nodes and storage", "cluster", client.ObjectKeyFromObject(c))
			continue
		}
		// The annotation lists every node the cluster could use, whichever it chose, for the hook to
		// narrow. Whether a VM has a place at all is judged against the chosen ones.
		hosts, _ := nodesToPlace(online, level, nil, storage)
		_, blocked := nodesToPlace(online, level, chosen, storage)
		usable := placeable(online, storage)
		if sites != nil {
			hosts = sites.Hosts(c, usable, level)
		}
		hosts, short := fitMemory(ctx, p.Client, p.ClassNamespace, c, usable, hosts)
		p.markShort(ctx, c, short)
		p.record(ctx, c, hosts, blocked)
	}
}

// record writes on the cluster the nodes its VMs may use, and why none can take a new VM, when
// they differ from what it holds.
func (p *Placer) record(ctx context.Context, c *clusterv1.Cluster, hosts []string, blocked string) {
	log := ctrl.LoggerFrom(ctx).WithName("placer")
	value := strings.Join(hosts, ",")
	if c.Annotations[OnlineNodesAnnotation] == value && c.Annotations[PlacementBlockedAnnotation] == blocked {
		return
	}
	var blockedValue *string
	if blocked != "" {
		blockedValue = &blocked
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: map[string]any{
		OnlineNodesAnnotation: value, PlacementBlockedAnnotation: blockedValue,
	}}})
	if err != nil {
		log.Error(err, "encoding the online Proxmox VE nodes", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	target := &clusterv1.Cluster{}
	target.Namespace, target.Name = c.Namespace, c.Name
	if err := p.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil && ctx.Err() == nil {
		log.Error(err, "recording the online Proxmox VE nodes", "cluster", client.ObjectKeyFromObject(c))
	}
}

// markSkipped records on the cluster the nodes left out of placement for want of a status, and
// clears the record once they answer.
func (p *Placer) markSkipped(ctx context.Context, c *clusterv1.Cluster, skipped []string) {
	var cond *metav1.Condition
	have := meta.FindStatusCondition(c.Status.Conditions, NodesSkippedCondition)
	if len(skipped) > 0 {
		cond = &metav1.Condition{
			Status: metav1.ConditionTrue, Reason: "StatusUnreadable",
			Message: fmt.Sprintf("%d Proxmox VE node(s) left out of placement because their status could not be read: %s", len(skipped), strings.Join(skipped, ", ")),
		}
		if have != nil && have.Message == cond.Message {
			return
		}
	} else if have == nil {
		return
	}
	if _, err := markCluster(ctx, p.Client, p.Client, client.ObjectKeyFromObject(c), NodesSkippedCondition, cond); err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "recording the nodes left out of placement", "cluster", client.ObjectKeyFromObject(c))
	}
}

// placement is where capmox may put a cluster's new VMs: the chosen nodes that are online, or every
// online node when none were chosen. While every chosen node is down it stays the chosen ones, so
// that nothing moves to a node the cluster was kept off.
func placement(chosen, online []string) []string {
	if len(chosen) == 0 {
		return online
	}
	up := slices.DeleteFunc(slices.Clone(chosen), func(n string) bool { return !slices.Contains(online, n) })
	if len(up) == 0 {
		return chosen
	}
	return up
}

// GeneratePatches gives capmox the nodes it may put the cluster's new VMs on and the template
// build each machine is cloned from (nodeImages), and has RKE2 and k3s nodes install their release
// from files checked against its pins (ReleaseChecksums). capmox clones every VM onto its template's node when the
// cluster names none, and fails to place any VM while one of the nodes it names does not answer,
// so the cluster always names the nodes that are online.
func (h *Hooks) GeneratePatches(ctx context.Context, req *runtimehooksv1.GeneratePatchesRequest, resp *runtimehooksv1.GeneratePatchesResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	cluster, err := h.requestCluster(ctx, req.Variables)
	if err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	chosen, _, err := optionalVariable[[]string](cluster, allowedNodesVariable)
	if err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	images := &nodeImages{hooks: h, cluster: cluster}
	for i := range req.Items {
		item := &req.Items[i]
		var patch []byte
		switch {
		case item.HolderReference.Kind == "Cluster" && item.HolderReference.FieldPath == "spec.infrastructureRef":
			patch, err = h.placementPatch(ctx, cluster, chosen)
		case kindOf(item) == proxmoxMachineTemplate:
			patch, err = images.patch(ctx, item)
		case talosContractPaths[kindOf(item)] != "":
			patch, err = images.contract(ctx, item)
		case h.Releases != nil:
			patch, err = h.Releases.patch(ctx, item, cluster)
		}
		if err != nil {
			resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
			return
		}
		if patch != nil {
			resp.Items = append(resp.Items, runtimehooksv1.GeneratePatchesResponseItem{
				UID: item.UID, PatchType: runtimehooksv1.JSONPatchType, Patch: patch,
			})
		}
	}
}

// requestCluster is the Cluster a patch request is for. Cluster API gives an external patch only
// the variables its own extension declares, none of the ClusterClass's, so the hook reads the
// cluster's variables from the Cluster itself.
func (h *Hooks) requestCluster(ctx context.Context, variables []runtimehooksv1.Variable) (*clusterv1.Cluster, error) {
	var key client.ObjectKey
	for _, v := range variables {
		if v.Name != runtimehooksv1.BuiltinsName {
			continue
		}
		var b runtimehooksv1.Builtins
		if err := json.Unmarshal(v.Value.Raw, &b); err != nil {
			return nil, fmt.Errorf("variable %s: %w", runtimehooksv1.BuiltinsName, err)
		}
		if b.Cluster != nil {
			key = client.ObjectKey{Namespace: b.Cluster.Namespace, Name: b.Cluster.Name}
		}
	}
	if key.Name == "" {
		return nil, errors.New("the patch request names no cluster")
	}
	c := &clusterv1.Cluster{}
	if err := h.Reader.Get(ctx, key, c); err != nil {
		return nil, fmt.Errorf("reading cluster %s: %w", key, err)
	}
	return c, nil
}

// placementPatch names the nodes on the cluster's ProxmoxClusterTemplate. It is nil while there
// are none to name.
func (h *Hooks) placementPatch(ctx context.Context, cluster *clusterv1.Cluster, chosen []string) ([]byte, error) {
	online, blocked, err := h.onlineNodes(ctx, cluster, chosen)
	if err != nil {
		return nil, err
	}
	if blocked != "" {
		return nil, errors.New(blocked)
	}
	nodes := placement(chosen, online)
	if len(nodes) == 0 {
		return nil, nil
	}
	patch, err := json.Marshal([]map[string]any{addOp(templateSpec+"/allowedNodes", nodes)})
	if err != nil {
		return nil, fmt.Errorf("patching the nodes: %w", err)
	}
	return patch, nil
}

// addOp is a JSON patch operation adding value at path.
func addOp(path string, value any) map[string]any {
	return map[string]any{"op": "add", "path": path, valueKey: value}
}

// onlineNodes are the nodes recorded on the cluster, or, for a cluster the Placer has not got to
// yet, such as one being created, those Proxmox VE reports now that run its node OS, or Talos.
func (h *Hooks) onlineNodes(ctx context.Context, cluster *clusterv1.Cluster, chosen []string) ([]string, string, error) {
	if value, ok := cluster.Annotations[OnlineNodesAnnotation]; ok {
		blocked := cluster.Annotations[PlacementBlockedAnnotation]
		if value == "" {
			return nil, blocked, nil
		}
		return strings.Split(value, ","), blocked, nil
	}
	if h.Nodes == nil {
		return nil, "", nil
	}
	level, err := clusterCPULevel(cluster)
	if err != nil {
		return nil, "", err
	}
	online, err := h.Nodes.OnlineNodes(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("reading the Proxmox VE nodes: %w", err)
	}
	_, storage, err := placementVariables(cluster)
	if err != nil {
		return nil, "", err
	}
	hosts, _ := nodesToPlace(online, level, nil, storage)
	_, blocked := nodesToPlace(online, level, chosen, storage)
	usable := placeable(online, storage)
	if h.Sites != nil {
		sites, err := h.Sites.Sites(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("reading the Proxmox VE storages and templates: %w", err)
		}
		hosts = sites.Hosts(cluster, usable, level)
	}
	hosts, _ = fitMemory(ctx, h.Reader, h.ClassNamespace, cluster, usable, hosts)
	return hosts, blocked, nil
}
