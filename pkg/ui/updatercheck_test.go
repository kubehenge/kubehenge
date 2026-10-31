package ui

import (
	"net/http"
	"testing"
)

// The appliance's updater asks whether the interface serves without signing in.
func TestHealthRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := h.browser().do("GET", "/api/health", "")
	expect(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Errorf("answered %q", rec.Body)
	}
}
