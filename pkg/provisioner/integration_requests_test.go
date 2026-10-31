//go:build integration

package provisioner

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/test/integration"
)

// RKE2's control planes configure the charts of RKE2's own that leave a container without a CPU
// request: canal's flannel and the snapshot controller.
func TestIntegrationRKE2RequestsForItsCharts(t *testing.T) {
	t.Parallel()
	want := map[string][]any{"rke2-canal": {"flannel"}, "rke2-snapshot-controller": {controllerKey}}
	seen := map[string]bool{}
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ConfigMap" || o.GetLabels()["kubehenge.dev/class"] == "" {
			continue
		}
		data, _, err := unstructured.NestedString(o.Object, "data", "templates.yaml")
		if err != nil {
			t.Fatal(err)
		}
		for _, tpl := range integration.Decode(t, []byte(data)) {
			if tpl.GetKind() != "RKE2ControlPlaneTemplate" {
				continue
			}
			files, _, err := unstructured.NestedSlice(tpl.Object, "spec", "template", "spec", "files")
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				file, ok := f.(map[string]any)
				if !ok {
					continue
				}
				for chart, path := range want {
					if file["path"] != "/var/lib/rancher/rke2/server/manifests/"+chart+"-config.yaml" {
						continue
					}
					seen[chart] = true
					content, ok := file["content"].(string)
					if !ok {
						t.Fatalf("%s: content %v", chart, file["content"])
					}
					var config helmChartConfig
					var values map[string]any
					if err := yaml.Unmarshal([]byte(content), &config); err != nil {
						t.Fatal(err)
					}
					if err := yaml.Unmarshal([]byte(config.Spec.ValuesContent), &values); err != nil {
						t.Fatal(err)
					}
					if config.Kind != "HelmChartConfig" {
						t.Errorf("%s: a %s", chart, config.Kind)
					}
					checkRequestsCPU(t, chart, values, path)
				}
			}
		}
	}
	for chart := range want {
		if !seen[chart] {
			t.Errorf("RKE2's control planes leave %s's values as they are", chart)
		}
	}
}

// The kube-vip that holds the cluster's API address on each control plane requests CPU, in every
// class that runs it.
func TestIntegrationControlPlaneKubeVipRequestsCPU(t *testing.T) {
	t.Parallel()
	found := map[string]bool{}
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ClusterClass" {
			continue
		}
		patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range jsonPatches(t, patches) {
			raw, _, _ := unstructured.NestedString(op, "valueFrom", "template")
			if !strings.Contains(raw, "/kube-vip.yaml") {
				continue
			}
			found[o.GetName()] = true
			tpl, err := template.New(o.GetName()).Funcs(sprig.HermeticTxtFuncMap()).Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := tpl.Execute(&out, map[string]any{"controlPlaneEndpoint": "192.0.2.50"}); err != nil {
				t.Fatal(err)
			}
			var file struct {
				Content string `json:"content"`
			}
			var pod map[string]any
			if err := yaml.Unmarshal(out.Bytes(), &file); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(file.Content), &pod); err != nil {
				t.Fatal(err)
			}
			checkRequestsCPU(t, o.GetName()+"'s kube-vip", lookup[map[string]any](t, pod, "spec", "containers", 0), []any{})
		}
	}
	for _, class := range []string{"kubehenge-kubeadm", "kubehenge-rke2", "kubehenge-k3s"} {
		if !found[class] {
			t.Errorf("%s runs no kube-vip on its control planes", class)
		}
	}
}
