package ui

import (
	"encoding/json"
	"maps"
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// poolChange adds a worker pool or changes one: its workers, and its size, which JSON null sets
// back to the cluster's workerMachine. A pool the cluster lacks is added, and needs its workers.
type poolChange struct {
	Replicas *int32          `json:"replicas"`
	Machine  json.RawMessage `json:"machine"`
}

// applyPools adds, changes and removes worker pools as a change asks, by name, a null removing one.
// It says what is wrong with the change, or returns "". The API server checks the names and sizes
// as it checks a cluster applied with kubectl.
func applyPools(c *clusterv1.Cluster, pools map[string]*poolChange) string {
	workers := &c.Spec.Topology.Workers
	for _, name := range slices.Sorted(maps.Keys(pools)) {
		change := pools[name]
		i := slices.IndexFunc(workers.MachineDeployments, func(md clusterv1.MachineDeploymentTopology) bool { return md.Name == name })
		switch {
		case change == nil && i < 0:
			return "the cluster has no worker pool " + name
		case change == nil:
			workers.MachineDeployments = slices.Delete(workers.MachineDeployments, i, i+1)
			continue
		case i < 0:
			if problem := provisioner.PoolNameProblem(c.Name, name); problem != "" {
				return problem
			}
			if change.Replicas == nil {
				return "Set the workers of the new pool " + name + "."
			}
			workers.MachineDeployments = append(workers.MachineDeployments, clusterv1.MachineDeploymentTopology{Class: workerClass, Name: name})
			i = len(workers.MachineDeployments) - 1
		}
		md := &workers.MachineDeployments[i]
		if n := change.Replicas; n != nil {
			if !provisioner.ValidWorkers(*n) {
				return provisioner.WorkersRule + " in each pool"
			}
			if autoscalingOf(md.Metadata.Annotations) != nil {
				return "pool " + name + " is autoscaled. Switch autoscaling off to set its workers"
			}
			md.Replicas = n
		}
		if change.Machine != nil {
			setPoolOverride(md, workerMachineVariable, change.Machine)
		}
	}
	return ""
}

// poolOverride is the index of a pool's own value of a variable, or -1 when it takes the cluster's.
func poolOverride(md *clusterv1.MachineDeploymentTopology, name string) int {
	return slices.IndexFunc(md.Variables.Overrides, func(v clusterv1.ClusterVariable) bool { return v.Name == name })
}

// setPoolOverride sets a pool's own value of a variable, or with JSON null drops it.
func setPoolOverride(md *clusterv1.MachineDeploymentTopology, name string, value json.RawMessage) {
	overrides := &md.Variables.Overrides
	i := poolOverride(md, name)
	switch {
	case string(value) == "null" && i >= 0:
		*overrides = slices.Delete(*overrides, i, i+1)
	case string(value) == "null":
	case i >= 0:
		(*overrides)[i].Value = apiextensionsv1.JSON{Raw: value}
	default:
		*overrides = append(*overrides, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: value}})
	}
}

// poolsEffect is what a change of pools does to the machines: a new size replaces the pool's
// workers. Adding or removing a pool, or setting its workers, only adds or removes some.
func poolsEffect(before *clusterv1.Cluster, pools map[string]*poolChange) string {
	for name, change := range pools {
		if change == nil || change.Machine == nil {
			continue
		}
		i := slices.IndexFunc(before.Spec.Topology.Workers.MachineDeployments, func(md clusterv1.MachineDeploymentTopology) bool { return md.Name == name })
		if i < 0 {
			continue
		}
		md := &before.Spec.Topology.Workers.MachineDeployments[i]
		was := json.RawMessage("null")
		if j := poolOverride(md, workerMachineVariable); j >= 0 {
			was = md.Variables.Overrides[j].Value.Raw
		}
		if !sameJSON(was, change.Machine) {
			return effectWorkers
		}
	}
	return effectScales
}
