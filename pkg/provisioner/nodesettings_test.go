package provisioner

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func nodesVariable(raw string) clusterv1.ClusterVariable {
	return clusterv1.ClusterVariable{Name: WorkerNodesVariable, Value: apiextensionsv1.JSON{Raw: []byte(raw)}}
}

func TestAPoolsNodeSettingsAreItsOwnElseTheClusters(t *testing.T) {
	t.Parallel()
	c := &clusterv1.Cluster{}
	c.Spec.Topology.Variables = []clusterv1.ClusterVariable{nodesVariable(`{"labels":[{"key":"team","value":"a"}]}`)}

	own, err := PoolNodeSettings(c, []clusterv1.ClusterVariable{nodesVariable(`{"taints":[{"key":"dedicated","value":"db","effect":"NoSchedule"}]}`)})
	if err != nil || len(own.Labels) != 0 || len(own.Taints) != 1 {
		t.Errorf("a pool's own value: %+v, %v", own, err)
	}
	inherited, err := PoolNodeSettings(c, nil)
	if err != nil || inherited.LabelList() != "team=a" || len(inherited.Taints) != 0 {
		t.Errorf("the cluster's value: %+v, %v", inherited, err)
	}
	none, err := PoolNodeSettings(&clusterv1.Cluster{}, nil)
	if err != nil || none.LabelList() != "" || none.TaintList() != "" {
		t.Errorf("no value: %+v, %v", none, err)
	}
	if _, err := PoolNodeSettings(c, []clusterv1.ClusterVariable{nodesVariable(`[1]`)}); err == nil {
		t.Error("a value that is no object was read")
	}
}

func TestNodeSettingsRenderForTheAutoscalerAndNodes(t *testing.T) {
	t.Parallel()
	n := NodeSettings{
		Labels: []NodeLabel{{Key: "tier", Value: "db"}, {Key: "team"}},
		Taints: []NodeTaint{{Key: "spot", Effect: "PreferNoSchedule"}, {Key: "dedicated", Value: "db", Effect: "NoSchedule"}},
	}
	if got := n.LabelList(); got != "team=,tier=db" {
		t.Errorf("labels %q", got)
	}
	if got := n.TaintList(); got != "dedicated=db:NoSchedule,spot=:PreferNoSchedule" {
		t.Errorf("taints %q", got)
	}
	core := n.CoreTaints()
	if len(core) != 2 || core[1] != (corev1.Taint{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}) && core[0] != (corev1.Taint{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}) {
		t.Errorf("core taints %+v", core)
	}
}
