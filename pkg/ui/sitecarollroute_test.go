package ui

import (
	"net/http"
	"strings"
	"testing"
)

func TestSiteCARollRouteChecksTheAuthorityAndNeedsTheSetupPrivileges(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	// No management cluster is up yet, so nothing rolls and the route still answers.
	if rec := b.do("POST", "/api/setup/site-ca-roll", `{"siteCA":""}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"inherits":0`) {
		t.Errorf("before the first setup: %d %s", rec.Code, rec.Body.String())
	}
	expect(t, b.do("POST", "/api/setup/site-ca-roll", `{"siteCA":"not a certificate"}`), http.StatusUnprocessableEntity)
	if rec := b.do("POST", "/api/setup/site-ca-roll", `{"siteCA":""}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"inherits"`) {
		t.Errorf("an empty site CA answered %d %s", rec.Code, rec.Body.String())
	}
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/setup/site-ca-roll", `{"siteCA":""}`), http.StatusForbidden)
}
