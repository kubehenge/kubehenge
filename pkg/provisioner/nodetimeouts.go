package provisioner

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// MaxNodeTimeoutSeconds is the longest wait a machine's deletion may be given for a node's drain,
// its volumes' detach or the Node's removal. A day is longer than any rollout should be held.
const MaxNodeTimeoutSeconds = 24 * 60 * 60

// NodeTimeoutRule is the message for a timeout out of range.
var NodeTimeoutRule = fmt.Sprintf("timeouts: 0 for never, or up to %d seconds", MaxNodeTimeoutSeconds)

// NodeTimeouts are the waits a machine's deletion gives a node before it goes on without it, as
// Cluster API's topology holds them. Unset, a timeout is the ClusterClass's.
type NodeTimeouts struct {
	// Drain bounds the eviction of the node's pods. The pods left are deleted with the machine.
	Drain *int32 `json:"nodeDrainTimeoutSeconds,omitempty"`
	// VolumeDetach bounds the wait for the node's volumes to detach.
	VolumeDetach *int32 `json:"nodeVolumeDetachTimeoutSeconds,omitempty"`
	// Deletion bounds the attempts to delete the node's Node object.
	Deletion *int32 `json:"nodeDeletionTimeoutSeconds,omitempty"`
}

// NodeTimeoutsOf reads the timeouts of a control plane's topology.
func NodeTimeoutsOf(d *clusterv1.ControlPlaneTopologyMachineDeletionSpec) NodeTimeouts {
	return NodeTimeouts{Drain: d.NodeDrainTimeoutSeconds, VolumeDetach: d.NodeVolumeDetachTimeoutSeconds, Deletion: d.NodeDeletionTimeoutSeconds}
}

// NodeTimeoutsOfPool reads the timeouts of a worker pool's topology.
func NodeTimeoutsOfPool(d *clusterv1.MachineDeploymentTopologyMachineDeletionSpec) NodeTimeouts {
	return NodeTimeouts{Drain: d.NodeDrainTimeoutSeconds, VolumeDetach: d.NodeVolumeDetachTimeoutSeconds, Deletion: d.NodeDeletionTimeoutSeconds}
}

// SetOn replaces the timeouts of a control plane's topology with these.
func (t NodeTimeouts) SetOn(d *clusterv1.ControlPlaneTopologyMachineDeletionSpec) {
	d.NodeDrainTimeoutSeconds, d.NodeVolumeDetachTimeoutSeconds, d.NodeDeletionTimeoutSeconds = t.Drain, t.VolumeDetach, t.Deletion
}

// SetOnPool replaces the timeouts of a worker pool's topology with these, and leaves the pool's
// deletion order as it is.
func (t NodeTimeouts) SetOnPool(d *clusterv1.MachineDeploymentTopologyMachineDeletionSpec) {
	d.NodeDrainTimeoutSeconds, d.NodeVolumeDetachTimeoutSeconds, d.NodeDeletionTimeoutSeconds = t.Drain, t.VolumeDetach, t.Deletion
}

// Problem says what is wrong with the timeouts, or returns "".
func (t NodeTimeouts) Problem() string {
	if t.errors(nil) != nil {
		return NodeTimeoutRule
	}
	return ""
}

// errors lists each timeout out of range, under path.
func (t NodeTimeouts) errors(path *field.Path) field.ErrorList {
	var errs field.ErrorList
	for _, f := range []struct {
		name string
		v    *int32
	}{{"nodeDrainTimeoutSeconds", t.Drain}, {"nodeVolumeDetachTimeoutSeconds", t.VolumeDetach}, {"nodeDeletionTimeoutSeconds", t.Deletion}} {
		if f.v != nil && (*f.v < 0 || *f.v > MaxNodeTimeoutSeconds) {
			errs = append(errs, field.Invalid(path.Child(f.name), *f.v, NodeTimeoutRule))
		}
	}
	return errs
}
