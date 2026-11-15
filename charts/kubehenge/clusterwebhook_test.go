package kubehenge_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"
)

type webhook struct {
	Name            string             `json:"name"`
	MatchConditions []policyExpression `json:"matchConditions"`
}

type webhookConfiguration struct {
	Kind     string    `json:"kind"`
	Webhooks []webhook `json:"webhooks"`
}

// clusterWebhook renders the chart and finds the webhook that checks Kubehenge's clusters.
func clusterWebhook(t *testing.T) webhook {
	t.Helper()
	for _, doc := range controllerDocuments(t) {
		var c webhookConfiguration
		if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
			t.Fatal(err)
		}
		if c.Kind != "ValidatingWebhookConfiguration" {
			continue
		}
		for _, w := range c.Webhooks {
			if w.Name == "clusters.kubehenge.dev" {
				return w
			}
		}
	}
	t.Fatal("the chart has no clusters.kubehenge.dev webhook")
	return webhook{}
}

// called tells whether the API server calls the webhook for the request, by its match conditions.
func called(t *testing.T, w webhook, operation string, object, oldObject map[string]any) bool {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range w.MatchConditions {
		ast, issues := env.Compile(m.Expression)
		if issues.Err() != nil {
			t.Fatalf("%s: %v", m.Expression, issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		var old any = oldObject
		if oldObject == nil {
			old = nil
		}
		got, _, err := program.Eval(map[string]any{"object": object, "oldObject": old, "request": map[string]any{"operation": operation}})
		if err != nil {
			t.Fatalf("%s: %v", m.Expression, err)
		}
		if matched, ok := got.Value().(bool); !ok || !matched {
			return false
		}
	}
	return true
}

// webhookCluster is a cluster as the webhook's conditions read it, with these annotations, labels
// and finalizers, and a spec of that version.
func webhookCluster(version string, annotations map[string]string, labels map[string]any, finalizers []any) map[string]any {
	meta := map[string]any{"name": "web", "namespace": "clusters"}
	if annotations != nil {
		a := map[string]any{}
		for k, v := range annotations {
			a[k] = v
		}
		meta["annotations"] = a
	}
	if labels != nil {
		meta["labels"] = labels
	}
	if finalizers != nil {
		meta["finalizers"] = finalizers
	}
	return map[string]any{"metadata": meta, "spec": map[string]any{"topology": map[string]any{
		"version": version, "classRef": map[string]any{"name": "kubehenge-talos"},
	}}}
}

// The cluster webhook is called for a new cluster, a changed spec, and a change to the annotations
// it checks. An update of anything else, kubehenge.dev/allow-deletion among it, never calls it, so
// a cluster whose controller is away can still be annotated and deleted.
func TestClusterWebhookSkipsUpdatesItDoesNotCheck(t *testing.T) {
	t.Parallel()
	w := clusterWebhook(t)
	const cilium, abandon = "kubehenge.dev/cilium-cluster-id", "kubehenge.dev/abandon-deletion"
	base := map[string]string{cilium: "7"}
	with := func(extra map[string]string) map[string]string {
		a := maps.Clone(base)
		maps.Copy(a, extra)
		return a
	}
	before := webhookCluster("v1.36.5", base, nil, nil)
	for name, tc := range map[string]struct {
		operation string
		object    map[string]any
		old       map[string]any
		called    bool
	}{
		"a new cluster":        {"CREATE", before, nil, true},
		"allow-deletion added": {"UPDATE", webhookCluster("v1.36.5", with(map[string]string{"kubehenge.dev/allow-deletion": "true"}), nil, nil), before, false},
		"a label and a finalizer added": {
			"UPDATE", webhookCluster("v1.36.5", base, map[string]any{"team": "a"}, []any{"example.org/hold"}), before, false,
		},
		"nothing changed":           {"UPDATE", before, before, false},
		"the spec changed":          {"UPDATE", webhookCluster("v1.37.1", base, nil, nil), before, true},
		"the Cilium id changed":     {"UPDATE", webhookCluster("v1.36.5", map[string]string{cilium: "8"}, nil, nil), before, true},
		"the Cilium id removed":     {"UPDATE", webhookCluster("v1.36.5", map[string]string{}, nil, nil), before, true},
		"the Cilium id set":         {"UPDATE", before, webhookCluster("v1.36.5", nil, nil, nil), true},
		"abandon added, even empty": {"UPDATE", webhookCluster("v1.36.5", with(map[string]string{abandon: ""}), nil, nil), before, true},
	} {
		if got := called(t, w, tc.operation, tc.object, tc.old); got != tc.called {
			t.Errorf("%s: webhook called %v, want %v", name, got, tc.called)
		}
	}
}

// A cluster being deleted calls the webhook for a changed spec, which it refuses, and for a newly
// written abandon-deletion. Every other update, the finalizers and the annotations of the deletion
// among them, never calls it, so the deletion cannot stall.
func TestClusterWebhookIsCalledForASpecChangeOfAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	w := clusterWebhook(t)
	const abandon, allow = "kubehenge.dev/abandon-deletion", "kubehenge.dev/allow-deletion"
	deleting := func(c map[string]any) map[string]any {
		meta, ok := c["metadata"].(map[string]any)
		if !ok {
			t.Fatal("the cluster has no metadata")
		}
		meta["deletionTimestamp"] = "2026-11-15T10:00:00Z"
		return c
	}
	held := []any{"cluster.cluster.x-k8s.io", "kubehenge.dev/cluster"}
	before := deleting(webhookCluster("v1.36.5", map[string]string{allow: "true"}, nil, held))
	for name, tc := range map[string]struct {
		object map[string]any
		called bool
	}{
		"the spec changed":            {deleting(webhookCluster("v1.37.1", map[string]string{allow: "true"}, nil, held)), true},
		"a finalizer removed":         {deleting(webhookCluster("v1.36.5", map[string]string{allow: "true"}, nil, held[:1])), false},
		"every finalizer gone":        {deleting(webhookCluster("v1.36.5", map[string]string{allow: "true"}, nil, []any{})), false},
		"the deletion status written": {deleting(webhookCluster("v1.36.5", map[string]string{allow: "true", "kubehenge.dev/deletion-status": "{}"}, nil, held)), false},
		"abandon-deletion written":    {deleting(webhookCluster("v1.36.5", map[string]string{allow: "true", abandon: "web"}, nil, held)), true},
		"nothing changed":             {before, false},
	} {
		if got := called(t, w, "UPDATE", tc.object, before); got != tc.called {
			t.Errorf("%s: webhook called %v, want %v", name, got, tc.called)
		}
	}
}

// The deletion policy matches one version of Cluster, so the API server converts a delete through
// v1beta1, whose class has another shape, before the policy reads it.
func TestTheDeletionPolicyReadsOneVersion(t *testing.T) {
	t.Parallel()
	for _, r := range deletionPolicy(t).Spec.MatchConstraints.ResourceRules {
		if !slices.Equal(r.APIVersions, []string{"v1beta2"}) {
			t.Errorf("the policy matches versions %s, want v1beta2 alone", strings.Join(r.APIVersions, ","))
		}
	}
}
