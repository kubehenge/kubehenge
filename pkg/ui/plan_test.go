package ui

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// planPatches feed pod security to the control plane, logs to the workers and the load balancer
// to both, and leave monitoring to the controller.
func planPatches() []clusterv1.ClusterClassPatch {
	template := func(t string) []clusterv1.JSONPatch {
		return []clusterv1.JSONPatch{{Op: "add", Path: "/spec/template/spec/files/-", ValueFrom: &clusterv1.JSONPatchValue{Template: t}}}
	}
	controlPlane := clusterv1.PatchSelector{Kind: "RKE2ControlPlaneTemplate", MatchResources: clusterv1.PatchSelectorMatch{ControlPlane: new(true)}}
	workers := clusterv1.PatchSelector{
		Kind:           "RKE2ConfigTemplate",
		MatchResources: clusterv1.PatchSelectorMatch{MachineDeploymentClass: &clusterv1.PatchSelectorMatchMachineDeploymentClass{Names: []string{"worker"}}},
	}
	return []clusterv1.ClusterClassPatch{
		{Name: "admission", EnabledIf: `{{ if .podSecurity }}true{{ end }}`, Definitions: []clusterv1.PatchDefinition{
			{Selector: controlPlane, JSONPatches: template(`content: {{ .podSecurity.level | quote }} on {{ .builtin.cluster.name }}`)},
		}},
		{Name: "audit", Definitions: []clusterv1.PatchDefinition{
			{Selector: workers, JSONPatches: []clusterv1.JSONPatch{{Op: "add", Path: "/spec/x", ValueFrom: &clusterv1.JSONPatchValue{Variable: "logs.collector"}}}},
		}},
		{Name: "kube-vip", Definitions: []clusterv1.PatchDefinition{
			{Selector: controlPlane, JSONPatches: template(`{{- with .loadBalancer }}{{ .addresses }}{{ end }}`)},
			{Selector: workers, JSONPatches: template(`{{ printf "%s" .loadBalancer }}`)},
		}},
	}
}

func TestPlanSaysWhatAChangeReplacesWithoutMakingIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	class := &clusterv1.ClusterClass{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	class.Spec.Patches = planPatches()
	if err := h.kube.Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters/default/one/plan",
		`{"version":"v1.36.5+rke2r1","workerPools":{"md-0":3},"variables":{"monitoring":{"enabled":true},"podSecurity":{"level":"restricted"},"logs":{"collector":"node"},"loadBalancer":{"provider":"metallb","addresses":["10.0.0.50"]}}}`)
	expect(t, rec, http.StatusOK)
	var plan planView
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	// The version stays as it is, so it plans nothing.
	want := []plannedChange{
		{Setting: "workerPools", Effect: effectScales},
		{Setting: "loadBalancer", Effect: effectAll},
		{Setting: "logs", Effect: effectWorkers},
		{Setting: "monitoring", Effect: effectNone},
		{Setting: "podSecurity", Effect: effectControlPlane},
	}
	if !slices.Equal(plan.Changes, want) {
		t.Errorf("changes = %+v, want %+v", plan.Changes, want)
	}
	if plan.Cluster.WorkerPools[0].Replicas != 3 || string(plan.Cluster.Variables["podSecurity"]) != `{"level":"restricted"}` {
		t.Errorf("the planned cluster = %+v, want 3 workers and restricted pods", plan.Cluster)
	}

	kept := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, kept); err != nil {
		t.Fatal(err)
	}
	if *kept.Spec.Topology.Workers.MachineDeployments[0].Replicas != 2 || variableValue(kept, "monitoring") != nil {
		t.Errorf("the plan changed the cluster: %+v", kept.Spec.Topology)
	}

	// What the change would refuse, the plan refuses the same way.
	expect(t, b.do("POST", "/api/clusters/default/one/plan", `{"variables":{"cni":"cilium"}}`), http.StatusUnprocessableEntity)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/clusters/default/one/plan", `{"variables":{"monitoring":{"enabled":true}}}`), http.StatusForbidden)
}
