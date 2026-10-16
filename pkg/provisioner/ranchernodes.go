package provisioner

import (
	"encoding/json"
	"fmt"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// RancherMinimumCores is what Rancher's documentation asks of every node of the cluster it runs on.
// On fewer, Rancher's first start, etcd and the API server crowd one another out: the API server
// stops answering, kube-vip loses its lease and drops the cluster's address.
const RancherMinimumCores = 4

type machineCores struct {
	Cores *int `json:"cores,omitempty"`
}

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
	cores, err := nodeCores(cluster, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides)
	if err != nil {
		return err
	}
	if cores < RancherMinimumCores {
		small = append(small, fmt.Sprintf("its control planes have %d", cores))
	}
	for i := range topology.Workers.MachineDeployments {
		md := &topology.Workers.MachineDeployments[i]
		if md.Replicas != nil && *md.Replicas == 0 {
			continue
		}
		cores, err := nodeCores(cluster, class, "workerMachine", md.Variables.Overrides)
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

// nodeCores is the vCPUs a machine variable gives its nodes: from a machine deployment's or the
// control plane's override, else the cluster's value, else the class's default.
func nodeCores(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass, name string, overrides []clusterv1.ClusterVariable) (int, error) {
	for _, vars := range [][]clusterv1.ClusterVariable{overrides, cluster.Spec.Topology.Variables} {
		for _, v := range vars {
			if v.Name != name {
				continue
			}
			var m machineCores
			if err := json.Unmarshal(v.Value.Raw, &m); err != nil {
				return 0, fmt.Errorf("variable %s: %w", name, err)
			}
			if m.Cores != nil {
				return *m.Cores, nil
			}
			return classDefaultCores(class, name)
		}
	}
	return classDefaultCores(class, name)
}

func classDefaultCores(class *clusterv1.ClusterClass, name string) (int, error) {
	for i := range class.Spec.Variables {
		v := &class.Spec.Variables[i]
		if v.Name != name {
			continue
		}
		schema := v.Schema.OpenAPIV3Schema
		if d := schema.Properties["cores"].Default; d != nil {
			var cores int
			if err := json.Unmarshal(d.Raw, &cores); err != nil {
				return 0, fmt.Errorf("class %s, variable %s: default cores: %w", class.Name, name, err)
			}
			return cores, nil
		}
		if schema.Default != nil {
			var m machineCores
			if err := json.Unmarshal(schema.Default.Raw, &m); err == nil && m.Cores != nil {
				return *m.Cores, nil
			}
		}
	}
	return 0, fmt.Errorf("class %s gives %s no default vCPUs", class.Name, name)
}
