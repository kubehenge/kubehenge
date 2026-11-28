package ui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

// The warning's button adds the job with the user's rights, and the warning goes.
func TestAddingTheAppliancesBackupJobEndsTheWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	h.server.ApplianceVMID = 151
	d := h.browser()
	d.signIn("dev@pve")
	if rec := d.do("POST", "/api/appliance/backup-job", ""); rec.Code != http.StatusForbidden || len(h.accounts.backedUp) != 0 {
		t.Errorf("without Sys.Modify: %d %s", rec.Code, rec.Body)
	}
	a := h.browser()
	a.signIn("admin@pve")
	if got := a.do("GET", "/api/setup", "").Body.String(); !strings.Contains(got, `"backup":false`) {
		t.Fatalf("setup = %s", got)
	}
	if rec := a.do("POST", "/api/appliance/backup-job", ""); rec.Code != http.StatusNoContent || len(h.accounts.backedUp) != 1 || h.accounts.backedUp[0] != 151 {
		t.Fatalf("added: %d %s, %v", rec.Code, rec.Body, h.accounts.backedUp)
	}
	if got := a.do("GET", "/api/setup", "").Body.String(); !strings.Contains(got, `"backup":true`) {
		t.Errorf("after the job: setup = %s", got)
	}
}
