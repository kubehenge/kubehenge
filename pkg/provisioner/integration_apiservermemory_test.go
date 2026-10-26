//go:build integration

package provisioner

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Each class gives its control planes' API server a Go memory limit out of the control plane's own
// memory: 40 % where it runs as a pod of its own, half of k3s's whole server process.
func TestIntegrationClassesLimitTheAPIServersMemory(t *testing.T) {
	t.Parallel()
	vars := map[string]any{"controlPlaneMachine": map[string]any{"memoryMiB": float64(4096)}}
	want := map[string]struct{ path, value string }{
		"kubehenge-kubeadm": {"/spec/template/spec/kubeadmConfigSpec/clusterConfiguration/apiServer/extraEnvs", "- name: GOMEMLIMIT\n  value: 1638MiB\n"},
		"kubehenge-rke2":    {"/spec/template/spec/serverConfig/kubeAPIServer/extraEnv", "GOMEMLIMIT: 1638MiB\n"},
		"kubehenge-k3s":     {"/spec/template/spec/kthreesConfigSpec/files/-", "Environment=GOMEMLIMIT=2048MiB\n"},
		"kubehenge-talos":   {"/spec/template/spec/controlPlaneConfig/controlplane/strategicPatches/-", `"apiVersion: v1alpha1\nkind: KubeAPIServerConfig\nenv:\n  GOMEMLIMIT: 1638MiB"`},
	}
	found := map[string]bool{}
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		w, ok := want[o.GetName()]
		if o.GetKind() != "ClusterClass" || !ok {
			continue
		}
		patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range jsonPatches(t, patches) {
			valueFrom, ok := op["valueFrom"].(map[string]any)
			if !ok || op["path"] != w.path {
				continue
			}
			text, ok := valueFrom["template"].(string)
			if !ok || !strings.Contains(text, "GOMEMLIMIT") {
				continue
			}
			if got := capiTemplate(t, text, vars); !strings.Contains(got, w.value) {
				t.Errorf("%s: %q, without %q", o.GetName(), got, w.value)
			}
			found[o.GetName()] = true
		}
	}
	for class := range want {
		if !found[class] {
			t.Errorf("%s gives its API server no memory limit", class)
		}
	}
}
