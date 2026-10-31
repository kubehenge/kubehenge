package provisioner

import (
	"context"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// reachClass feeds podSecurity to the control plane and logs to the workers.
func reachClass() *clusterv1.ClusterClass {
	return &clusterv1.ClusterClass{Spec: clusterv1.ClusterClassSpec{Patches: []clusterv1.ClusterClassPatch{
		{Name: "admission", Definitions: []clusterv1.PatchDefinition{{
			Selector:    clusterv1.PatchSelector{MatchResources: clusterv1.PatchSelectorMatch{ControlPlane: new(true)}},
			JSONPatches: []clusterv1.JSONPatch{{Op: "add", Path: "/spec/x", ValueFrom: &clusterv1.JSONPatchValue{Template: `{{ .podSecurity.level }}`}}},
		}}},
		{Name: "audit", Definitions: []clusterv1.PatchDefinition{{
			Selector: clusterv1.PatchSelector{MatchResources: clusterv1.PatchSelectorMatch{
				MachineDeploymentClass: &clusterv1.PatchSelectorMatchMachineDeploymentClass{Names: []string{"worker"}},
			}},
			JSONPatches: []clusterv1.JSONPatch{{Op: "add", Path: "/spec/x", ValueFrom: &clusterv1.JSONPatchValue{Variable: "logs.collector"}}},
		}}},
	}}}
}

func TestRollsOnlyControlPlane(t *testing.T) {
	t.Parallel()
	one := func(replicas int32, variables ...clusterv1.ClusterVariable) *clusterv1.Cluster {
		c := cluster(variables...)
		c.Spec.Topology.ControlPlane.Replicas = new(replicas)
		return c
	}
	variable := func(name, raw string) clusterv1.ClusterVariable {
		return clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}}
	}
	newer := one(1)
	newer.Spec.Topology.Version = "v1.37.2+rke2r1"
	for _, tc := range []struct {
		name     string
		old, now *clusterv1.Cluster
		want     bool
	}{
		{"a new version", one(1), newer, true},
		{"a control-plane variable set", one(1), one(1, variable("podSecurity", `{"level":"restricted"}`)), true},
		{"a control-plane variable taken away", one(1, variable("podSecurity", `{"level":"restricted"}`)), one(1), true},
		{"a workers' variable", one(1), one(1, variable("logs", `{"collector":"node"}`)), false},
		{"three control planes", one(3), one(3, variable("podSecurity", `{"level":"restricted"}`)), false},
		{"a new cluster", nil, newer, false},
	} {
		if got := rollsOnlyControlPlane(tc.old, tc.now, reachClass()); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	reach := VariableReach(reachClass())
	if !reach["podSecurity"].ControlPlane || reach["podSecurity"].Workers || !reach["logs"].Workers || reach["logs"].ControlPlane {
		t.Errorf("reach %+v", reach)
	}
}

// kubectl hears of a roll of the only control plane as the interface's plan does.
func TestClusterValidatorWarnsOfASingleControlPlaneRoll(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	old := networkCluster("192.168.1.1", 16)
	old.Spec.Topology.ControlPlane.Replicas = new(int32(1))
	newer := old.DeepCopy()
	newer.Spec.Topology.Version = "v1.37.2+rke2r1"
	warnings, err := v.ValidateUpdate(context.Background(), old, newer)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal([]string(warnings), []string{SingleControlPlaneRoll}) {
		t.Errorf("warnings %q", warnings)
	}
	if warnings, _ := v.ValidateUpdate(context.Background(), old, old.DeepCopy()); len(warnings) != 0 {
		t.Errorf("warnings %q for no change", warnings)
	}
}
