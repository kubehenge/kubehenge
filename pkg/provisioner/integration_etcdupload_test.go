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
)

// capiTemplate runs a patch's template as Cluster API does, with sprig's functions over the
// cluster's variables.
func capiTemplate(t *testing.T, text string, variables map[string]any) string {
	t.Helper()
	tpl, err := template.New("patch").Funcs(sprig.TxtFuncMap()).Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, variables); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// RKE2's and k3s's classes give their servers the drop-in that uploads their own snapshots only
// while a cluster asks for it, naming the Secret with the target and no key of the store; kubeadm's
// has no such patch.
func TestIntegrationChartEtcdUpload(t *testing.T) {
	t.Parallel()
	on := map[string]any{"etcdSnapshots": map[string]any{
		"enabled": true, "target": "store", "schedule": "0 */6 * * *", "keep": 7, "distributionUpload": true,
	}}
	found := map[string]bool{}
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ClusterClass" {
			continue
		}
		patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range patches {
			patch, ok := p.(map[string]any)
			if !ok {
				t.Fatalf("%s: a patch of %T", o.GetName(), p)
			}
			if name, named := patch["name"].(string); !named || !strings.HasSuffix(name, "-etcd-upload") {
				continue
			}
			found[o.GetName()] = true
			enabledIf, ok := patch["enabledIf"].(string)
			if !ok {
				t.Fatalf("%s: the upload is always on", o.GetName())
			}
			for _, vars := range []map[string]any{
				{},
				{"etcdSnapshots": map[string]any{"enabled": true, "target": "store"}},
				{"etcdSnapshots": map[string]any{"enabled": false, "distributionUpload": true}},
			} {
				if got := capiTemplate(t, enabledIf, vars); got != "" {
					t.Errorf("%s uploads with %v", o.GetName(), vars)
				}
			}
			if got := capiTemplate(t, enabledIf, on); got != "true" {
				t.Errorf("%s does not upload when asked: %q", o.GetName(), got)
			}
			ops := jsonPatches(t, []any{patch})
			if len(ops) != 1 {
				t.Fatalf("%s: %d operations", o.GetName(), len(ops))
			}
			valueFrom, ok := ops[0]["valueFrom"].(map[string]any)
			if !ok {
				t.Fatalf("%s: no template for the drop-in: %v", o.GetName(), ops[0])
			}
			text, ok := valueFrom["template"].(string)
			if !ok {
				t.Fatalf("%s: no template for the drop-in: %v", o.GetName(), valueFrom)
			}
			var file struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := yaml.Unmarshal([]byte(capiTemplate(t, text, on)), &file); err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := yaml.Unmarshal([]byte(file.Content), &settings); err != nil {
				t.Fatalf("%s: %v\n%s", o.GetName(), err, file.Content)
			}
			want := map[string]any{
				"etcd-s3": true, "etcd-s3-config-secret": EtcdUploadSecret,
				"etcd-snapshot-schedule-cron": "0 */6 * * *", "etcd-snapshot-retention": float64(7),
			}
			for k, v := range want {
				if settings[k] != v {
					t.Errorf("%s: %s is %v, not %v", o.GetName(), k, settings[k], v)
				}
			}
			if len(settings) != len(want) || !strings.HasPrefix(file.Path, "/etc/rancher/") {
				t.Errorf("%s writes %s: %v", o.GetName(), file.Path, settings)
			}
		}
	}
	if !found["kubehenge-rke2"] || !found["kubehenge-k3s"] || found["kubehenge-kubeadm"] {
		t.Errorf("classes with the upload: %v", found)
	}
}
