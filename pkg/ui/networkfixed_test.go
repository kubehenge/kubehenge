package ui

import (
	"net/http"
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
