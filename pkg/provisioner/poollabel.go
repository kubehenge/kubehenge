package provisioner

import (
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// DefaultPool is the worker pool every cluster made through the interface starts with.
const DefaultPool = "md-0"

// DefaultPoolLabel is what the interface, the CLI and the controller's messages call DefaultPool. A
// pool's name is a DNS label, so a user's pool can match it only in lower case.
const DefaultPoolLabel = "Workers"

// PoolLabel is a pool's name as people know it among the cluster's pools: DefaultPoolLabel for
// DefaultPool, unless a user's pool would read the same, which leaves both their names.
func PoolLabel(name string, pools []string) string {
	if name == DefaultPool && !slices.Contains(pools, strings.ToLower(DefaultPoolLabel)) {
		return DefaultPoolLabel
	}
	return name
}

// TopologyPools are the names of the cluster's worker pools.
func TopologyPools(c *clusterv1.Cluster) []string {
	names := make([]string, len(c.Spec.Topology.Workers.MachineDeployments))
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		names[i] = c.Spec.Topology.Workers.MachineDeployments[i].Name
	}
	return names
}
