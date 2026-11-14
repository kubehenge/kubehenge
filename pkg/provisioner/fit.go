package provisioner

import (
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Fit is the most a cluster's machines ask of Proxmox VE: the memory of the largest machine, the
// disks of every machine with each pool at its most workers, and the nodes and storage it chose.
type Fit struct {
	LargestMiB int64
	DiskGiB    int64
	// Pools are the control planes, then each worker pool, with the disk of one machine.
	Pools   []PoolFit
	Nodes   []string
	Storage string
}

// PoolFit is one pool's share of a Fit. Name is empty for the control planes.
type PoolFit struct {
	Name     string
	Machines int64
	DiskGiB  int64
}

// FitOf reads what a cluster's machines ask for, with the class's sizes where the cluster sets none.
func FitOf(c *clusterv1.Cluster, class *clusterv1.ClusterClass) (Fit, error) {
	var f Fit
	largest, err := largestMachineMiB(c, class)
	if err != nil {
		return f, err
	}
	f.LargestMiB = largest
	topology := &c.Spec.Topology
	controlPlanes := int64(1)
	if topology.ControlPlane.Replicas != nil {
		controlPlanes = int64(*topology.ControlPlane.Replicas)
	}
	shape, err := nodeShape(c, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides)
	if err != nil {
		return f, err
	}
	f.Pools = append(f.Pools, PoolFit{Machines: controlPlanes, DiskGiB: diskOf(shape)})
	for i := range topology.Workers.MachineDeployments {
		md := &topology.Workers.MachineDeployments[i]
		shape, err = nodeShape(c, class, "workerMachine", md.Variables.Overrides)
		if err != nil {
			return f, err
		}
		f.Pools = append(f.Pools, PoolFit{Name: md.Name, Machines: int64(workerRange(md).most), DiskGiB: diskOf(shape)})
	}
	for _, p := range f.Pools {
		f.DiskGiB += p.Machines * p.DiskGiB
	}
	f.Nodes, f.Storage, err = placementVariables(c)
	if err != nil {
		return f, fmt.Errorf("reading the cluster's nodes and storage: %w", err)
	}
	return f, nil
}

func diskOf(m machineSize) int64 {
	if m.DiskGiB == nil {
		return 0
	}
	return int64(*m.DiskGiB)
}

// NeededGiB words a machine's memory in GiB, as the placement's condition does.
func NeededGiB(mib int64) string { return neededGiB(mib) }
