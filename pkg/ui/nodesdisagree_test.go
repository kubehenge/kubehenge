package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func TestARefusedFreshTicketTellsTheUserTheNodesDisagreeAboutTheTime(t *testing.T) {
	t.Parallel()
	s := &Server{}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"nodes disagree": {pve.ErrNodesDisagree, "Proxmox VE nodes disagree about the time. Compare timedatectl on each node and read pvecm status."},
		"session ended":  {pve.ErrAuthentication, "session expired. Sign in again"},
	} {
		rec := httptest.NewRecorder()
		s.proxmoxError(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody), c.err)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want 401 with %q", name, rec.Code, rec.Body.String(), c.want)
		}
	}
}
