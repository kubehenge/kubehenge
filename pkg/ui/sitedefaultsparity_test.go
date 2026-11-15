package ui

import (
	"net/http"
	"strings"
	"testing"
)

// The interface's save and the CLI's settings command send the same request, so one refusal
// serves both.
func TestSetupRefusesABadSiteCAAndTimeZone(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	for field, want := range map[string]string{
		`"siteCA":"not a certificate"`: "Site CA",
		`"timeZone":"Mars/Base"`:       "Time zone",
	} {
		rec := b.do("POST", "/api/setup", strings.Replace(preflightApply, `"settings":{`, `"settings":{`+field+`,`, 1))
		expect(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: the refusal = %s, want it to name %q", field, rec.Body.String(), want)
		}
	}
	if h.server.Appliance.Current() != nil {
		t.Error("a run started with a bad site default")
	}
}
