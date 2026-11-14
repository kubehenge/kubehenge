package provisioner

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// AllowedNodesRefusal is why the nodes a cluster's VMs are kept to are refused, or "" when they are
// fine: there is at least one, none twice, and each is known. A node in kept, one the cluster
// already lists, stays without being known, as one the person changing it may not see. A nil known
// accepts every node.
func AllowedNodesRefusal(nodes, kept []string, known func(node string) bool) string {
	if len(nodes) == 0 {
		return "allowed nodes: choose at least one Proxmox VE node"
	}
	for i, node := range nodes {
		if slices.Contains(nodes[:i], node) {
			return fmt.Sprintf("allowed nodes: %s is listed twice", node)
		}
		if known != nil && !slices.Contains(kept, node) && !known(node) {
			return "allowed nodes: no Proxmox VE node " + node
		}
	}
	return ""
}

// validateAllowedNodes holds the nodes a cluster's VMs are kept to by AllowedNodesRefusal, when the
// cluster is created or the list changes. An unset list means every node and is not judged here.
func (v *ClusterValidator) validateAllowedNodes(ctx context.Context, reads *pveReads, old, c *clusterv1.Cluster) field.ErrorList {
	if old != nil && !variableChanged(old, c, allowedNodesVariable) {
		return nil
	}
	nodes, _, err := optionalVariable[[]string](c, allowedNodesVariable)
	if err != nil || nodes == nil {
		// The class's schema refuses a list that is not strings.
		return nil
	}
	var kept []string
	if old != nil {
		kept, _, _ = optionalVariable[[]string](old, allowedNodesVariable)
	}
	var known func(string) bool
	if v.Hooks.Sites != nil {
		sites, readErr := reads.lookAtSites(ctx)
		if readErr != nil {
			_ = admitOnReadFailure(ctx, readErr)
		} else if sites.Nodes != nil {
			known = func(node string) bool { return slices.Contains(sites.Nodes, node) }
		}
	}
	if msg := AllowedNodesRefusal(nodes, kept, known); msg != "" {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "topology", "variables").Key(allowedNodesVariable), nodes, msg)}
	}
	return nil
}
