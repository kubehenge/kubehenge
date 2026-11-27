package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeKubernetesRegistry serves the releases given, each signed or not, as registry.k8s.io does: a
// tag per release and the tag sha256-<digest>.sig beside a signed image. It counts the requests.
func fakeKubernetesRegistry(t *testing.T, releases map[string]bool) (*ReleaseSignatures, *atomic.Int32) {
	t.Helper()
	digest := func(path string) string {
		sum := sha256.Sum256([]byte(path))
		return hex.EncodeToString(sum[:])
	}
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		image, ref, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
		if !ok || r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if _, known := releases[ref]; known {
			w.Header().Set("Docker-Content-Digest", "sha256:"+digest(image+ref))
			return
		}
		for v, signed := range releases {
			if signed && ref == "sha256-"+digest(image+v)+".sig" {
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &ReleaseSignatures{Registry: srv.URL, HTTP: srv.Client()}, &asked
}

func TestReleaseSignaturesRefuseAnUnsignedRelease(t *testing.T) {
	t.Parallel()
	s, asked := fakeKubernetesRegistry(t, map[string]bool{
		"v1.34.2": true,
		"v1.35.0": false, "v1.35.1": false, "v1.35.2": false, "v1.35.3": true,
		"v1.33.5": true, "v1.33.6": false, "v1.33.7": true,
	})
	for version, want := range map[string]string{
		"v1.34.2": "",
		"v1.35.0": "v1.35.0 has no signature of Kubernetes' release. Use v1.35.3",
		"v1.33.6": "v1.33.6 has no signature of Kubernetes' release. Use v1.33.7",
		"v1.99.0": "v1.99.0 is no Kubernetes release",
	} {
		note, err := s.Check(t.Context(), version)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != want || note != "" {
			t.Errorf("Check(%s) = %q, %q, want %q", version, note, got, want)
		}
	}
	// Each answer is kept: asking again reads nothing.
	before := asked.Load()
	if _, err := s.Check(t.Context(), "v1.35.0"); err == nil || asked.Load() != before {
		t.Errorf("asking again: %v after %d more requests", err, asked.Load()-before)
	}
}

func TestReleaseSignaturesLeaveAReleaseWhenTheRegistryDoesNotAnswer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	s := &ReleaseSignatures{Registry: srv.URL, HTTP: srv.Client()}
	note, err := s.Check(t.Context(), "v1.35.0")
	if err != nil || !strings.HasPrefix(note, "release signature unchecked: ") || strings.Contains(note, "\n") {
		t.Fatalf("Check() = %q, %v", note, err)
	}
}
