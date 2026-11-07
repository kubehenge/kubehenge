package ui

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const pinnedK3s = "v1.99.9+k3s1"

// releaseServer publishes the checksum file and install script of pinnedK3s.
func releaseServer(t *testing.T) *httptest.Server {
	t.Helper()
	sum := strings.Repeat("c", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sha256sum-amd64.txt") && strings.Contains(r.URL.Path, pinnedK3s):
			_, _ = w.Write([]byte(sum + "  k3s\n" + sum + "  k3s-airgap-images-amd64.tar.zst\n"))
		case strings.HasSuffix(r.URL.Path, "/install.sh") && strings.Contains(r.URL.Path, pinnedK3s):
			_, _ = w.Write([]byte("#!/bin/sh\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func pinHarness(t *testing.T) (*harness, *lockedBuffer) {
	t.Helper()
	h := newHarness(t)
	srv := releaseServer(t)
	h.server.PinChecks = &provisioner.ReleaseChecksums{HTTP: srv.Client(), GitHub: srv.URL, Raw: srv.URL}
	logs := &lockedBuffer{}
	h.server.Log = slog.New(slog.NewTextHandler(logs, nil))
	return h, logs
}

func TestAdministratorPinsAReleaseAfterSeeingItsDigests(t *testing.T) {
	t.Parallel()
	h, logs := pinHarness(t)
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("POST", "/api/release-pins/check", `{"version":"`+pinnedK3s+`"}`)
	expect(t, rec, http.StatusOK)
	var shown releasePinView
	if err := json.Unmarshal(rec.Body.Bytes(), &shown); err != nil || len(shown.File) != 64 || len(shown.Script) != 64 {
		t.Fatalf("digests shown: %s", rec.Body)
	}
	expect(t, a.do("POST", "/api/release-pins", `{"version":"`+pinnedK3s+`","file":"`+shown.File+`","script":"`+shown.Script+`"}`), http.StatusCreated)
	rec = a.do("GET", "/api/release-pins", "")
	expect(t, rec, http.StatusOK)
	var listed releasePinsView
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Pins) != 1 || listed.Pins[0].By != "admin@pve" || listed.Pins[0].Version != pinnedK3s {
		t.Fatalf("pins listed: %s", rec.Body)
	}
	// The change reaches the activity log through the user the line is logged with.
	if out := logs.String(); !strings.Contains(out, "pinned a release") || !strings.Contains(out, "user=admin@pve") {
		t.Errorf("log: %s", out)
	}
	expect(t, a.do("DELETE", "/api/release-pins/"+strings.ReplaceAll(pinnedK3s, "+", "%2B"), ""), http.StatusNoContent)
	if out := logs.String(); !strings.Contains(out, "removed a release pin") {
		t.Errorf("the removal is not logged: %s", out)
	}
}

func TestPinningRefusesABadVersionAndAMissingChecksumFile(t *testing.T) {
	t.Parallel()
	h, _ := pinHarness(t)
	a := h.browser()
	a.signIn("admin@pve")
	for _, version := range []string{"1.36", "v1.98.0+k3s1"} {
		expect(t, a.do("POST", "/api/release-pins/check", `{"version":"`+version+`"}`), http.StatusUnprocessableEntity)
	}
	stale := `{"version":"` + pinnedK3s + `","file":"` + strings.Repeat("0", 64) + `","script":"` + strings.Repeat("0", 64) + `"}`
	expect(t, a.do("POST", "/api/release-pins", stale), http.StatusConflict)
}

func TestOnlyAdministratorsChangeReleasePins(t *testing.T) {
	t.Parallel()
	h, _ := pinHarness(t)
	d := h.browser()
	d.signIn("dev@pve")
	expect(t, d.do("GET", "/api/release-pins", ""), http.StatusForbidden)
	expect(t, d.do("POST", "/api/release-pins/check", `{"version":"`+pinnedK3s+`"}`), http.StatusForbidden)
	expect(t, d.do("POST", "/api/release-pins", `{"version":"`+pinnedK3s+`"}`), http.StatusForbidden)
	expect(t, d.do("DELETE", "/api/release-pins/"+strings.ReplaceAll(pinnedK3s, "+", "%2B"), ""), http.StatusForbidden)
}
