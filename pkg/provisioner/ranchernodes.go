package provisioner

import (
	"fmt"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// RancherMinimumCores is what Rancher's documentation asks of every node of the cluster it runs on.
// On fewer, Rancher's first start, etcd and the API server crowd one another out: the API server
// stops answering, kube-vip loses its lease and drops the cluster's address.
const RancherMinimumCores = 4

// checkRancherNodes refuses a cluster that runs Rancher on nodes with fewer vCPUs than Rancher
// needs: its control planes, and the workers of every machine deployment that has any, since
// Rancher's pods run there too.
func checkRancherNodes(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	r, err := rancherOf(cluster)
	if err != nil || !r.Install {
		return err
	}
	topology := cluster.Spec.Topology
	var small []string
	cores, _, err := nodeSize(cluster, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides)
	if err != nil {
		return err
	}
	if cores < RancherMinimumCores {
		small = append(small, fmt.Sprintf("its control planes have %d", cores))
	}
	for i := range topology.Workers.MachineDeployments {
		md := &topology.Workers.MachineDeployments[i]
		if workerRange(md).most == 0 {
			continue
		}
		cores, _, err := nodeSize(cluster, class, "workerMachine", md.Variables.Overrides)
		if err != nil {
			return err
		}
		if cores < RancherMinimumCores {
			small = append(small, fmt.Sprintf("workers %s have %d", md.Name, cores))
		}
	}
	if len(small) > 0 {
		return fmt.Errorf("every node of the cluster Rancher runs on needs at least %d vCPUs, and %s",
			RancherMinimumCores, strings.Join(small, ", "))
	}
	return nil
}
