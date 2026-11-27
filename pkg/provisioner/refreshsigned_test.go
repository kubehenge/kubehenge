package provisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestTemplateRefreshBuildsNoUnsignedKubernetesRelease(t *testing.T) {
	t.Parallel()
	// The registry holds v1.36.7 without its release signature, and no other release.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifests/v1.36.7") {
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	r, _, planned := patchRefresh(t, map[int]string{36: "v1.36.7"})
	r.Signatures = &templates.ReleaseSignatures{Registry: srv.URL, HTTP: srv.Client()}
	r.Missing = []templates.Request{{Distribution: templates.Kubeadm, KubernetesVersion: "v1.36.7", NodeOS: templates.DefaultNodeOS}}
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "v1.36.7 has no signature of Kubernetes' release") {
		t.Errorf("Run() = %v", err)
	}
	want := []string{"kubehenge-rke2-almalinux-10", "kubehenge-kubeadm-v1.36.5-almalinux-10"}
	if !slices.Equal(*planned, want) {
		t.Errorf("planned %v, want %v", *planned, want)
	}
}
