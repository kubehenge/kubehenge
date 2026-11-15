package ui

import (
	"net/http"
	"strings"
	"testing"
)

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
