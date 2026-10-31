//go:build integration

package provisioner

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubehenge/kubehenge/test/integration"
)

// Every k3s cluster goes without k3s's local path provisioner, whose StorageClass would claim the
// default beside the Proxmox CSI plugin's, and without servicelb while the cluster's own load
// balancer answers for its Services.
func TestIntegrationK3sDisablesLocalStorage(t *testing.T) {
	t.Parallel()
	const name = "k3s-load-balancer"
	disabled := []string{"spec", "template", "spec", "kthreesConfigSpec", "serverConfig", "disableComponents"}
	var always []any
	var found map[string]any
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		switch {
		case o.GetKind() == "ClusterClass" && o.GetName() == "kubehenge-k3s":
			patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range patches {
				if patch, ok := p.(map[string]any); ok && patch["name"] == name {
					found = patch
				}
			}
		case o.GetKind() == "ConfigMap" && o.GetLabels()["kubehenge.dev/class"] != "":
			data, _, err := unstructured.NestedString(o.Object, "data", "templates.yaml")
			if err != nil {
				t.Fatal(err)
			}
			for _, tpl := range integration.Decode(t, []byte(data)) {
				if tpl.GetKind() != "KThreesControlPlaneTemplate" {
					continue
				}
				components, ok, err := unstructured.NestedSlice(tpl.Object, disabled...)
				if err != nil || !ok {
					t.Fatalf("%s %s disables nothing: %v", tpl.GetKind(), tpl.GetName(), err)
				}
				always = components
			}
		}
	}
	if want := []any{"local-storage"}; !reflect.DeepEqual(always, want) {
		t.Errorf("the k3s control plane disables %v, want %v", always, want)
	}
	if found == nil {
		t.Fatalf("the k3s class has no %s patch", name)
	}
	enabledIf, ok := found["enabledIf"].(string)
	if !ok {
		t.Fatalf("%s is always on", name)
	}
	for _, tc := range []struct {
		loadBalancer map[string]any
		on           string
	}{
		{map[string]any{"enabled": true}, "true"},
		{map[string]any{"enabled": false}, ""},
		{nil, ""},
	} {
		if got := capiTemplate(t, enabledIf, map[string]any{"loadBalancer": tc.loadBalancer}); got != tc.on {
			t.Errorf("%s with load balancer %v: %q, want %q", name, tc.loadBalancer, got, tc.on)
		}
	}
	// The patch adds to the template's list, so local-storage stays disabled beside servicelb.
	ops := jsonPatches(t, []any{found})
	if len(ops) != 1 || ops[0]["op"] != "add" || ops[0]["path"] != "/spec/template/spec/kthreesConfigSpec/serverConfig/disableComponents/-" || ops[0]["value"] != "servicelb" {
		t.Errorf("%s: %v, want servicelb added to the list", name, ops)
	}
}
