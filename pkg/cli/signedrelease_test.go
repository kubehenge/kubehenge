package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTemplateCreateRefusesAnUnsignedKubernetesRelease(t *testing.T) {
	t.Parallel()
	// The registry holds v1.35.0 without its release signature, and no other release.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifests/v1.35.0") {
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"template", "create", "kubeadm", "--kubernetes-version", "v1.35.0", "--node", "pve1"})
	err := Execute(context.WithValue(t.Context(), kubernetesRegistryKey{}, srv.URL), root)
	if err == nil || !strings.Contains(err.Error(), "--kubernetes-version: v1.35.0 has no signature of Kubernetes' release") {
		t.Fatalf("err = %v, output %s", err, out.String())
	}
}
