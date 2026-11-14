package ui

import (
	"net/http"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExportHoldsClustersAndTheirClasses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("POST", "/api/clusters", createBody), http.StatusCreated)
	ctx := t.Context()
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	raw, err := h.server.exportObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{"kind: Cluster\n", "name: web\n", "kind: ClusterClass\n", "name: " + c.Spec.Topology.ClassRef.Name + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the export lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "resourceVersion") || strings.Contains(out, "managedFields") {
		t.Errorf("the export carries server-side fields:\n%s", out)
	}
}
