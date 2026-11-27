package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// unsignedRegistry is a registry on 127.0.0.1 that holds Kubernetes v1.35.0 without its release
// signature, and no other release.
func unsignedRegistry(t *testing.T) *templates.ReleaseSignatures {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifests/v1.35.0") {
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &templates.ReleaseSignatures{Registry: srv.URL, HTTP: srv.Client()}
}

func TestUnsignedKubernetesReleaseIsRefusedBeforeItsBuild(t *testing.T) {
	t.Parallel()
	h := newHarness(t, refreshConfigMap("true", nil), requestsConfigMap())
	withTemplates(t, h)
	h.server.Signatures = unsignedRegistry(t)
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) {
		t.Error("an unsigned release was built")
		return 0, nil
	}
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("POST", "/api/templates", `{"distribution":"kubeadm","kubernetesVersion":"v1.35.0","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`)
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "v1.35.0 has no signature of Kubernetes' release") {
		t.Errorf("build: %s", rec.Body)
	}
	d := h.browser()
	d.signIn("dev@pve")
	rec = d.do("POST", "/api/template-builds", `{"distribution":"kubeadm","kubernetesVersion":"v1.35.0"}`)
	expect(t, rec, http.StatusUnprocessableEntity)
	if got := requested(t, h); len(got) != 0 || !strings.Contains(rec.Body.String(), "no signature") {
		t.Errorf("request: %s, requests %v", rec.Body, got)
	}
}
