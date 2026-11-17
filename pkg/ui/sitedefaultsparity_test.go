package ui

import (
	"net/http"
	"strings"
	"testing"
)

// The interface's save and the CLI's settings command send the same request, so one refusal
// serves both.
func TestSetupRefusesABadSiteCATimeZoneAndVMIDRange(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	for field, want := range map[string]string{
		`"siteCA":"not a certificate"`:                          "Site CA",
		`"timeZone":"Mars/Base"`:                                "Time zone",
		`"workloadVMIDRange":{"start":2000,"end":1000}`:         "VM ID range 2000-1000 starts above its end",
		`"workloadVMIDRange":{"start":99,"end":1000}`:           "VM ID range 99-1000 is outside 100 to 999999999",
		`"workloadVMIDRange":{"start":1000,"end":999999999999}`: "VM ID range",
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
