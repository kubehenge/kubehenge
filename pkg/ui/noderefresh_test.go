package ui

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

func refreshConfigMap(creatorBuilds string, status map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{Namespace: "kubehenge-system", Name: provisioner.TemplateRefreshName}
	cm.Data = map[string]string{"weekday": "Sun", "time": "01:00", "timeZone": "UTC", "soakHours": "24", "creatorBuilds": creatorBuilds}
	maps.Copy(cm.Data, status)
	return cm
}

func requestsConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{Namespace: "kubehenge-system", Name: provisioner.TemplateRequestsName}
}

func requested(t *testing.T, h *harness) map[string]provisioner.TemplateRequest {
	t.Helper()
	cm := requestsConfigMap()
	if err := h.kube.Get(context.Background(), client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatal(err)
	}
	out := map[string]provisioner.TemplateRequest{}
	for k, v := range cm.Data {
		var r provisioner.TemplateRequest
		if err := json.Unmarshal([]byte(v), &r); err != nil {
			t.Fatal(err)
		}
		out[k] = r
	}
	return out
}

func TestBuildNowAsksTheControllerForARefresh(t *testing.T) {
	t.Parallel()
	h := newHarness(t, refreshConfigMap("false", map[string]string{
		"state": "failed", "trigger": "schedule", "started": "2026-10-11T01:00:00Z", "finished": "2026-10-11T01:20:00Z",
		"lines": "kubehenge-ubuntu-26.04", "error": "building kubehenge-ubuntu-26.04: the mirror is down", "log": "01:00:00 building\n01:20:00 failed",
	}), requestsConfigMap())
	withTemplates(t, h)
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) { return 0, nil }
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("GET", "/api/templates", "")
	expect(t, rec, http.StatusOK)
	var got templatesView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	r := got.Refresh
	if r == nil || r.Weekday != "Sun" || r.SoakHours != 24 || r.Last == nil || r.Last.State != "failed" || len(r.Last.Log) != 2 || r.Next == nil {
		t.Fatalf("refresh = %s", rec.Body)
	}
	expect(t, a.do("POST", "/api/templates/refresh", ""), http.StatusAccepted)
	if got := requested(t, h); got[provisioner.RefreshRequest].By != "admin@pve" || len(got) != 1 {
		t.Fatalf("requests %v", got)
	}

	d := h.browser()
	d.signIn("dev@pve")
	expect(t, d.do("POST", "/api/templates/refresh", ""), http.StatusForbidden)
}

func TestBuildNowNeedsTheRefresh(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withTemplates(t, h)
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) { return 0, nil }
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("POST", "/api/templates/refresh", ""), http.StatusConflict)
}

func TestClusterCreatorsHaveMissingTemplatesBuiltWhenAllowed(t *testing.T) {
	t.Parallel()
	for _, allowed := range []bool{false, true} {
		creatorBuilds := "false"
		if allowed {
			creatorBuilds = "true"
		}
		h := newHarness(t, refreshConfigMap(creatorBuilds, map[string]string{"state": "running", "lines": "kubehenge-kubeadm-v1.37.1-ubuntu-26.04"}), requestsConfigMap())
		d := h.browser()
		d.signIn("dev@pve")
		rec := d.do("GET", "/api/template-builds", "")
		expect(t, rec, http.StatusOK)
		var v templateBuildsView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v.Allowed != allowed || len(v.Building) != 1 {
			t.Errorf("allowed %v: %s", allowed, rec.Body)
		}
		want := http.StatusForbidden
		if allowed {
			want = http.StatusAccepted
		}
		expect(t, d.do("POST", "/api/template-builds", `{"distribution":"kubeadm","kubernetesVersion":"v1.38.0"}`), want)
		if got := requested(t, h); allowed != (got["kubeadm-v1.38.0"].By == "dev@pve") {
			t.Errorf("allowed %v: requests %v", allowed, got)
		}
		if !allowed {
			continue
		}
		expect(t, d.do("POST", "/api/template-builds", `{"distribution":"k3s"}`), http.StatusAccepted)
		for _, body := range []string{`{"distribution":"talos"}`, `{"distribution":"kubeadm","kubernetesVersion":"1.38"}`, `{"distribution":"rke2","kubernetesVersion":"v1.38.0"}`} {
			expect(t, d.do("POST", "/api/template-builds", body), http.StatusUnprocessableEntity)
		}
		if got := requested(t, h); len(got) != 2 || got[provisioner.CloudImageLine].By != "dev@pve" {
			t.Errorf("requests %v", got)
		}
		// Seeing a cluster is not creating one.
		v2 := h.browser()
		v2.signIn("viewer@pve")
		expect(t, v2.do("POST", "/api/template-builds", `{"distribution":"k3s"}`), http.StatusForbidden)
	}
}

func TestClusterShowsItsNodeImages(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	line, err := provisioner.MachineLine("rke2", "v1.36.5+rke2r1")
	if err != nil {
		t.Fatal(err)
	}
	c := cluster("default", "one")
	c.Annotations = map[string]string{
		provisioner.NodeImageAnnotation:          day(9).Format(time.RFC3339),
		provisioner.NodeImageRefreshedAnnotation: "build-202610010000 to build-202610080000 at 2026-10-09T00:00:00Z",
	}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "upgrades", Value: apiextensionsv1.JSON{Raw: []byte(`{"mode":"off","window":{"weekdays":["Sat"],"start":"02:00"}}`)},
	})
	h := newHarness(t, c, refreshConfigMap("false", nil))
	class := &clusterv1.ClusterClass{}
	if err = h.kube.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	builds, err := provisioner.NodeImageBuilds{templates.LineKey(line): {day(1), day(8), day(12)}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	class.Annotations[provisioner.NodeImageBuildsAnnotation] = builds
	if err = h.kube.Update(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var got clusterView
	if err = json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	n := got.NodeImage
	// The harness's clock is Tuesday the 13th; the build of the 12th is offered on the 13th, and
	// the window next opens on Saturday the 17th.
	if n == nil || !n.Refreshes || len(n.Builds) != 1 || n.Builds[0].Build != "build-202610080000" || n.Builds[0].Template != "kubehenge-ubuntu-26.04" ||
		n.Next == nil || n.Next.Build != "build-202610120000" || !n.Next.Due.Equal(time.Date(2026, 10, 17, 2, 0, 0, 0, time.UTC)) || n.Last == "" {
		t.Fatalf("node image = %s", rec.Body)
	}
	// Once the upgrader holds the cluster, its decision is what the page shows.
	held := &clusterv1.Cluster{}
	if err = h.kube.Get(context.Background(), client.ObjectKeyFromObject(c), held); err != nil {
		t.Fatal(err)
	}
	held.Annotations[provisioner.NodeImageHeldAnnotation] = "build-202610120000 at 2026-10-17T02:00:05Z"
	if err = h.kube.Update(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	rec = a.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	got = clusterView{}
	if err = json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if n = got.NodeImage; n == nil || n.Next == nil || !n.Next.Due.Equal(time.Date(2026, 10, 17, 2, 0, 5, 0, time.UTC)) {
		t.Fatalf("held node image = %s", rec.Body)
	}
}
