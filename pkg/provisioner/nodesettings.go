package provisioner

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// WorkerNodesVariable holds the labels and taints the nodes of a worker pool register with: the
// cluster's value for every pool, which a pool's own replaces.
const WorkerNodesVariable = "workerNodes"

// NodeSettings are a worker pool's node labels and taints (the workerNodes variable).
type NodeSettings struct {
	Labels []NodeLabel `json:"labels,omitempty"`
	Taints []NodeTaint `json:"taints,omitempty"`
}

// NodeLabel is a label the pool's nodes register with.
type NodeLabel struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// NodeTaint is a taint the pool's nodes register with.
type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// PoolNodeSettings reads the node settings of a worker pool: its own override of the variable, else
// the cluster's value, else none.
func PoolNodeSettings(cluster *clusterv1.Cluster, overrides []clusterv1.ClusterVariable) (NodeSettings, error) {
	var out NodeSettings
	for _, vars := range [][]clusterv1.ClusterVariable{overrides, cluster.Spec.Topology.Variables} {
		v := variableNamed(vars, WorkerNodesVariable)
		if v == nil {
			continue
		}
		if err := json.Unmarshal(v.Value.Raw, &out); err != nil {
			return NodeSettings{}, fmt.Errorf("variable %s: %w", WorkerNodesVariable, err)
		}
		break
	}
	return out, nil
}

// LabelList renders the labels as the autoscaler and the kubelet read them: key=value, comma
// separated, by key.
func (n NodeSettings) LabelList() string {
	parts := make([]string, 0, len(n.Labels))
	for _, l := range n.Labels {
		parts = append(parts, l.Key+"="+l.Value)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// TaintList renders the taints as the autoscaler reads them: key=value:effect, comma separated, by
// key.
func (n NodeSettings) TaintList() string {
	parts := make([]string, 0, len(n.Taints))
	for _, t := range n.Taints {
		parts = append(parts, t.Key+"="+t.Value+":"+t.Effect)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// CoreTaints are the taints as Kubernetes holds them on a node.
func (n NodeSettings) CoreTaints() []corev1.Taint {
	out := make([]corev1.Taint, 0, len(n.Taints))
	for _, t := range n.Taints {
		out = append(out, corev1.Taint{Key: t.Key, Value: t.Value, Effect: corev1.TaintEffect(t.Effect)})
	}
	return out
}
