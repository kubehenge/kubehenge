//go:build integration

package provisioner

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubehenge/kubehenge/test/integration"
)

// The k3s class runs kube-proxy in nftables mode on every node while flannel and k3s's network
// policy controller are the cluster's: the controller writes the iptables filter table back whole,
// and takes kube-proxy's chains with it when they share it.
func TestIntegrationK3sKubeProxyOwnTable(t *testing.T) {
	t.Parallel()
	const name = "k3s-kube-proxy-nftables"
	paths := map[string]string{
		"KThreesControlPlaneTemplate": "/spec/template/spec/kthreesConfigSpec/agentConfig/kubeProxyArgs",
		"KThreesConfigTemplate":       "/spec/template/spec/agentConfig/kubeProxyArgs",
	}
	var found map[string]any
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ClusterClass" || o.GetName() != "kubehenge-k3s" {
			continue
		}
		patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range patches {
			if patch, ok := p.(map[string]any); ok && patch["name"] == name {
				found = patch
			}
		}
	}
	if found == nil {
		t.Fatalf("the k3s class has no %s patch", name)
	}
	enabledIf, ok := found["enabledIf"].(string)
	if !ok {
		t.Fatalf("%s is always on", name)
	}
	for cni, on := range map[string]string{"flannel": "true", "cilium": "", "calico": ""} {
		if got := capiTemplate(t, enabledIf, map[string]any{"cni": cni}); got != on {
			t.Errorf("%s with %s: %q, want %q", name, cni, got, on)
		}
	}
	definitions, _, err := unstructured.NestedSlice(found, "definitions")
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != len(paths) {
		t.Fatalf("%s: %d definitions, want one per template kind", name, len(definitions))
	}
	for _, d := range definitions {
		definition, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("definition %v", d)
		}
		kind, _, err := unstructured.NestedString(definition, "selector", "kind")
		if err != nil {
			t.Fatal(err)
		}
		ops := jsonPatches(t, []any{map[string]any{"definitions": []any{definition}}})
		if len(ops) != 1 {
			t.Fatalf("%s on %s: %d operations", name, kind, len(ops))
		}
		if ops[0]["op"] != "add" || ops[0]["path"] != paths[kind] {
			t.Errorf("%s on %s: %v %v, want add %s", name, kind, ops[0]["op"], ops[0]["path"], paths[kind])
		}
		if want := []any{"proxy-mode=nftables"}; !reflect.DeepEqual(ops[0]["value"], want) {
			t.Errorf("%s on %s: value %v, want %v", name, kind, ops[0]["value"], want)
		}
	}
}
