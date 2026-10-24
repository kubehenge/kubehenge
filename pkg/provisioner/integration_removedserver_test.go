//go:build integration

package provisioner

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Every RKE2 and k3s server is kept from starting again once its etcd member is removed: the
// drop-in names the distribution's own server unit and the tombstone in its own data directory.
func TestIntegrationClassesKeepRemovedServersDown(t *testing.T) {
	t.Parallel()
	servers := map[string]struct {
		files []string
		unit  string
		dir   string
	}{
		"RKE2ControlPlaneTemplate":    {[]string{"spec", "template", "spec", "files"}, "rke2-server", "rke2"},
		"KThreesControlPlaneTemplate": {[]string{"spec", "template", "spec", "kthreesConfigSpec", "files"}, "k3s", "k3s"},
	}
	seen := map[string]bool{}
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ConfigMap" || o.GetLabels()["kubehenge.dev/class"] == "" {
			continue
		}
		data, _, err := unstructured.NestedString(o.Object, "data", "templates.yaml")
		if err != nil {
			t.Fatal(err)
		}
		for _, tpl := range decodeAll(t, []byte(data)) {
			want, ok := servers[tpl.GetKind()]
			if !ok {
				continue
			}
			seen[tpl.GetKind()] = true
			files, _, err := unstructured.NestedSlice(tpl.Object, want.files...)
			if err != nil {
				t.Fatal(err)
			}
			path := "/etc/systemd/system/" + want.unit + ".service.d/10-kubehenge-removed.conf"
			check := "ExecStartPre=/bin/sh -c '! test -e /var/lib/rancher/" + want.dir + "/server/db/etcd/tombstone'\n"
			written := false
			for _, f := range files {
				file, ok := f.(map[string]any)
				if !ok || file["path"] != path {
					continue
				}
				written = true
				content, ok := file["content"].(string)
				if !ok || !strings.HasPrefix(content, "[Service]\n") || !strings.Contains(content, check) {
					t.Errorf("%s %s: %v", tpl.GetKind(), tpl.GetName(), file["content"])
				}
			}
			if !written {
				t.Errorf("%s %s writes no %s", tpl.GetKind(), tpl.GetName(), path)
			}
		}
	}
	for kind := range servers {
		if !seen[kind] {
			t.Errorf("no class has a %s", kind)
		}
	}
}
