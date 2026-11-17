package ui

import (
	"net/http"
	"strings"
	"testing"
)

func TestMirrorsAndAuthoritiesChangeAfterCreation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	admin := h.browser()
	admin.signIn("admin@pve")
	for _, change := range []string{
		`{"variables":{"registryMirrors":[{"registry":"docker.io","endpoints":["https://mirror.example.org"]}]}}`,
		`{"variables":{"releaseMirror":{"url":"https://releases.example.org"}}}`,
		`{"variables":{"trustedCAs":[]}}`,
	} {
		expect(t, admin.do("PATCH", "/api/clusters/default/one", change), http.StatusOK)
	}
}

// TestAChangeOffersNoRanges holds the interface to the rule the admission enforces: a cluster's pod
// and service ranges are set when it is created, so a change that names them is refused as it is
// sent, with the field named.
func TestAChangeOffersNoRanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	admin := h.browser()
	admin.signIn("admin@pve")
	for _, change := range []string{`{"pods":["172.20.0.0/16"]}`, `{"services":["172.21.0.0/16"]}`} {
		rec := admin.do("PATCH", "/api/clusters/default/one", change)
		expect(t, rec, http.StatusBadRequest)
		if body := rec.Body.String(); !strings.Contains(body, "malformed request") || !strings.Contains(body, "unknown field") {
			t.Errorf("%s: %s, want it refused as a field a change does not take", change, body)
		}
	}
}
