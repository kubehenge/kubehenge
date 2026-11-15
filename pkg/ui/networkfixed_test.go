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

func TestNetworkIsFixedForTheClustersLife(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	admin := h.browser()
	admin.signIn("admin@pve")
	rec := admin.do("PATCH", "/api/clusters/default/one", `{"variables":{"network":{"bridge":"vmbr9","vlan":30}}}`)
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "network cannot change after the cluster is created") {
		t.Fatalf("refusal = %s", rec.Body)
	}
}
