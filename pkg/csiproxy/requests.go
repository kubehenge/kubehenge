// Package csiproxy stands between a workload cluster's Proxmox VE CSI plugin and the Proxmox VE API
// in the clusters that take volume snapshots. Proxmox VE copies a volume only for root@pam, and
// root@pam's password never leaves the management cluster: the proxy passes every other call on
// with the cluster's own token, and turns a copy into a request in the workload cluster, which
// Kubehenge's controller in the management cluster carries out as root@pam once it has checked that
// both volumes are the cluster's own.
package csiproxy

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	// Namespace holds the copy requests, as ConfigMaps, in each workload cluster that takes volume
	// snapshots; nothing else is kept there, so the proxy's rights in it reach nothing else.
	Namespace = "kubehenge-volume-copies"
	// RequestLabel marks a ConfigMap in Namespace as a copy request.
	RequestLabel = "kubehenge.dev/volume-copy"

	// StateCopying is a request whose copy the controller started; a request without a state waits
	// for the controller.
	StateCopying = "copying"
	// StateDone is a request whose copy the controller finished.
	StateDone = "done"
	// StateFailed is a request the controller refused, or whose copy failed.
	StateFailed = "failed"

	keyNode       = "node"
	keyStorage    = "storage"
	keyVolume     = "volume"
	keyTarget     = "target"
	keyTargetNode = "targetNode"
	keyState      = "state"
	keyTask       = "task"
	keyMessage    = "message"
)

// Copy is a volume copy as the CSI plugin asks Proxmox VE for it: Volume on Storage on Node into
// Target on the same storage, on TargetNode when that is another node. Volume and Target are the
// volume names as the storage has them, without the storage's name.
type Copy struct {
	Node       string
	Storage    string
	Volume     string
	Target     string
	TargetNode string
}

// Destination is the node the copy lands on.
func (c *Copy) Destination() string {
	if c.TargetNode != "" {
		return c.TargetNode
	}
	return c.Node
}

// TargetID is the copy's Proxmox VE volume ID, storage:volume, as the storage lists it.
func (c *Copy) TargetID() string {
	return c.Storage + ":" + c.Target
}

// Request is a copy request as the workload cluster holds it.
type Request struct {
	Copy

	// Name is the ConfigMap's.
	Name string
	// State is one of StateCopying, StateDone and StateFailed, or empty while the request waits.
	State string
	// Task is the Proxmox VE task copying the volume, its UPID, once it started.
	Task string
	// Message says why a request failed.
	Message string
	Created time.Time
}

// Finished reports whether the controller is done with the request, either way.
func (r *Request) Finished() bool {
	return r.State == StateDone || r.State == StateFailed
}

// ParseRequest reads a copy request from its ConfigMap.
func ParseRequest(cm *corev1.ConfigMap) Request {
	d := cm.Data
	return Request{
		Node: d[keyNode], Storage: d[keyStorage], Volume: d[keyVolume], Target: d[keyTarget], TargetNode: d[keyTargetNode],
		Name: cm.Name, State: d[keyState], Task: d[keyTask], Message: d[keyMessage], Created: cm.CreationTimestamp.Time,
	}
}

// NewRequest is the ConfigMap that asks for a copy.
func NewRequest(c *Copy) *corev1.ConfigMap {
	data := map[string]string{keyNode: c.Node, keyStorage: c.Storage, keyVolume: c.Volume, keyTarget: c.Target}
	if c.TargetNode != "" {
		data[keyTargetNode] = c.TargetNode
	}
	return &corev1.ConfigMap{
		Namespace: Namespace, GenerateName: "copy-",
		Labels: map[string]string{RequestLabel: ""},
		Data:   data,
	}
}

// Record writes the controller's state of a request into its ConfigMap.
func Record(cm *corev1.ConfigMap, state, task, message string) {
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[keyState] = state
	delete(cm.Data, keyTask)
	delete(cm.Data, keyMessage)
	if task != "" {
		cm.Data[keyTask] = task
	}
	if message != "" {
		cm.Data[keyMessage] = message
	}
}
