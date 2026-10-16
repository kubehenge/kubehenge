package provisioner

import (
	"context"
	"encoding/json"
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

// OnlineNodesAnnotation lists, on each Cluster, the Proxmox VE nodes last seen online. Changing it
// has Cluster API compute the cluster's topology again, and with it the nodes capmox may use.
const OnlineNodesAnnotation = "kubehenge.dev/online-nodes"

// allowedNodesVariable is the ClusterClass variable naming the nodes a cluster's VMs keep to.
const allowedNodesVariable = "allowedNodes"

// Nodes tells which Proxmox VE nodes are online; *Proxmox is the real one.
type Nodes interface {
	OnlineNodes(ctx context.Context) ([]string, error)
}

// OnlineNodes lists the nodes Proxmox VE reports online, sorted.
func (p *Proxmox) OnlineNodes(ctx context.Context) ([]string, error) {
	var nodes []pveNode
	if err := p.Client.Get(ctx, "/nodes", &nodes); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	var online []string
	for _, n := range nodes {
		if n.Status == nodeOnline {
			online = append(online, n.Node)
		}
	}
	slices.Sort(online)
	return online, nil
}

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
	value := strings.Join(online, ",")
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !Owns(c, p.ClassNamespace) || !c.DeletionTimestamp.IsZero() || c.Annotations[OnlineNodesAnnotation] == value {
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

// GeneratePatches gives capmox the nodes it may put the cluster's new VMs on. capmox clones every
// VM onto its template's node when the cluster names none, and fails to place any VM while one of
// the nodes it names does not answer, so the cluster always names the nodes that are online.
func (h *Hooks) GeneratePatches(ctx context.Context, req *runtimehooksv1.GeneratePatchesRequest, resp *runtimehooksv1.GeneratePatchesResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	var chosen []string
	for _, v := range req.Variables {
		if v.Name != allowedNodesVariable {
			continue
		}
		if err := json.Unmarshal(v.Value.Raw, &chosen); err != nil {
			resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, fmt.Sprintf("variable %s: %v", allowedNodesVariable, err)
			return
		}
	}
	for i := range req.Items {
		item := &req.Items[i]
		holder := item.HolderReference
		if holder.Kind != "Cluster" || holder.FieldPath != "spec.infrastructureRef" {
			continue
		}
		online, err := h.onlineNodes(ctx, client.ObjectKey{Namespace: holder.Namespace, Name: holder.Name})
		if err != nil {
			resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
			return
		}
		nodes := placement(chosen, online)
		if len(nodes) == 0 {
			continue
		}
		patch, err := json.Marshal([]map[string]any{{"op": "add", "path": "/spec/template/spec/allowedNodes", "value": nodes}})
		if err != nil {
			resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
			return
		}
		resp.Items = append(resp.Items, runtimehooksv1.GeneratePatchesResponseItem{
			UID: item.UID, PatchType: runtimehooksv1.JSONPatchType, Patch: patch,
		})
	}
}

// onlineNodes are the nodes recorded on the cluster, or, for a cluster the Placer has not got to
// yet, such as one being created, those Proxmox VE reports now.
func (h *Hooks) onlineNodes(ctx context.Context, key client.ObjectKey) ([]string, error) {
	cluster := &clusterv1.Cluster{}
	if err := h.Reader.Get(ctx, key, cluster); err != nil {
		return nil, fmt.Errorf("reading cluster %s: %w", key, err)
	}
	if value, ok := cluster.Annotations[OnlineNodesAnnotation]; ok {
		if value == "" {
			return nil, nil
		}
		return strings.Split(value, ","), nil
	}
	if h.Nodes == nil {
		return nil, nil
	}
	online, err := h.Nodes.OnlineNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the Proxmox VE nodes: %w", err)
	}
	return online, nil
}
