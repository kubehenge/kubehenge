//go:build integration

package provisioner

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The Talos class asks Talos's flannel for network policies while it is the cluster's CNI, and
// takes flannel out for any other: Talos leaves policies unenforced unless told otherwise.
func TestIntegrationTalosFlannelPolicies(t *testing.T) {
	t.Parallel()
	want := map[string]map[string]string{
		"talos-flannel-policies": {"flannel": "true", "cilium": "", "calico": ""},
		"talos-cni":              {"flannel": "", "cilium": "true", "calico": "true"},
	}
	values := map[string]map[string]any{
		"talos-flannel-policies": {"apiVersion": "v1alpha1", "kind": "KubeFlannelCNIConfig", "kubeNetworkPoliciesEnabled": true},
		"talos-cni":              {"apiVersion": "v1alpha1", "kind": "KubeFlannelCNIConfig", "$patch": "delete"},
	}
	seen := map[string]bool{}
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ClusterClass" || o.GetName() != "kubehenge-talos" {
			continue
		}
		patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range patches {
			patch, ok := p.(map[string]any)
			if !ok {
				t.Fatalf("a patch of %T", p)
			}
			name, named := patch["name"].(string)
			cases, ok := want[name]
			if !named || !ok {
				continue
			}
			seen[name] = true
			enabledIf, ok := patch["enabledIf"].(string)
			if !ok {
				t.Fatalf("%s is always on", name)
			}
			for cni, on := range cases {
				if got := capiTemplate(t, enabledIf, map[string]any{"cni": cni}); got != on {
					t.Errorf("%s with %s: %q, want %q", name, cni, got, on)
				}
			}
			ops := jsonPatches(t, []any{patch})
			if len(ops) != 1 {
				t.Fatalf("%s: %d operations", name, len(ops))
			}
			text, ok := ops[0]["value"].(string)
			if !ok {
				t.Fatalf("%s: value %v", name, ops[0]["value"])
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(text), &got); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(got) != len(values[name]) {
				t.Errorf("%s: %v, want %v", name, got, values[name])
			}
			for k, v := range values[name] {
				if got[k] != v {
					t.Errorf("%s: %s is %v, want %v", name, k, got[k], v)
				}
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("the Talos class has no %s patch", name)
		}
	}
}
