package ui

import (
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// applyControlPlaneTimeouts sets the control planes' waits for a removed machine's node, or says
// what is wrong with them. Nil leaves them as they are.
func applyControlPlaneTimeouts(c *clusterv1.Cluster, t *provisioner.NodeTimeouts) string {
	if t == nil {
		return ""
	}
	if msg := t.Problem(); msg != "" {
		return msg
	}
	t.SetOn(&c.Spec.Topology.ControlPlane.Deletion)
	return ""
}

// timeoutsOnly tells whether a change of pools sets nothing but timeouts, which replace no machine
// and add or remove none.
func timeoutsOnly(pools map[string]*poolChange) bool {
	for _, change := range pools {
		if change == nil || change.Replicas != nil || change.Machine != nil {
			return false
		}
	}
	return true
}

// classControlPlaneTimeouts are the waits a class gives its control planes' removed machines.
func classControlPlaneTimeouts(c *clusterv1.ClusterClass) provisioner.NodeTimeouts {
	d := &c.Spec.ControlPlane.Deletion
	return provisioner.NodeTimeouts{Drain: d.NodeDrainTimeoutSeconds, VolumeDetach: d.NodeVolumeDetachTimeoutSeconds, Deletion: d.NodeDeletionTimeoutSeconds}
}

// classWorkerTimeouts are the waits a class gives its workers' removed machines.
func classWorkerTimeouts(c *clusterv1.ClusterClass) provisioner.NodeTimeouts {
	for i := range c.Spec.Workers.MachineDeployments {
		if md := &c.Spec.Workers.MachineDeployments[i]; md.Class == workerClass {
			d := &md.Deletion
			return provisioner.NodeTimeouts{Drain: d.NodeDrainTimeoutSeconds, VolumeDetach: d.NodeVolumeDetachTimeoutSeconds, Deletion: d.NodeDeletionTimeoutSeconds}
		}
	}
	return provisioner.NodeTimeouts{}
}
