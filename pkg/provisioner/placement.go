package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OnlineNodesAnnotation lists, on each Cluster, the Proxmox VE nodes last seen online whose CPU runs
// the cluster's node OS. Changing it has Cluster API compute the cluster's topology again, and with
// it the nodes capmox may use.
const OnlineNodesAnnotation = "kubehenge.dev/online-nodes"

// allowedNodesVariable is the ClusterClass variable naming the nodes a cluster's VMs keep to.
const allowedNodesVariable = "allowedNodes"

// Placer keeps OnlineNodesAnnotation current on every Kubehenge cluster.
type Placer struct {
	Client         client.Client
	ClassNamespace string
	Nodes          Nodes
	// Interval is how often the nodes are looked at: a node that goes down is left out of the
	// clusters' placement within about that long.
	Interval time.Duration
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
		o, err := ClusterNodeOS(c)
		if err != nil {
			log.Error(err, "reading the cluster's node OS", "cluster", client.ObjectKeyFromObject(c))
			continue
		}
		value := strings.Join(nodesFor(online, o), ",")
		if c.Annotations[OnlineNodesAnnotation] == value {
			continue
		}
		patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, OnlineNodesAnnotation, value)
		target := &clusterv1.Cluster{}
		target.Namespace, target.Name = c.Namespace, c.Name
		if err := p.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil && ctx.Err() == nil {
			log.Error(err, "recording the online Proxmox VE nodes", "cluster", client.ObjectKeyFromObject(c))
		}
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
// from checked files (ReleaseChecksums). capmox clones every VM onto its template's node when the
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

// placementPatch names the nodes on the cluster's ProxmoxClusterTemplate; it is nil while there
// are none to name.
func (h *Hooks) placementPatch(ctx context.Context, cluster *clusterv1.Cluster, chosen []string) ([]byte, error) {
	online, err := h.onlineNodes(ctx, cluster)
	if err != nil {
		return nil, err
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
// yet, such as one being created, those Proxmox VE reports now that run its node OS.
func (h *Hooks) onlineNodes(ctx context.Context, cluster *clusterv1.Cluster) ([]string, error) {
	if value, ok := cluster.Annotations[OnlineNodesAnnotation]; ok {
		if value == "" {
			return nil, nil
		}
		return strings.Split(value, ","), nil
	}
	if h.Nodes == nil {
		return nil, nil
	}
	o, err := ClusterNodeOS(cluster)
	if err != nil {
		return nil, err
	}
	online, err := h.Nodes.OnlineNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the Proxmox VE nodes: %w", err)
	}
	return nodesFor(online, o), nil
}
