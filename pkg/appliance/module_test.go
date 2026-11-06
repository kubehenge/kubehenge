package appliance

import (
	"slices"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// A user who signed in with an API token has no ticket for the module's provider to act with.
func TestTicketAuthUsesTheTokenOfATokenSession(t *testing.T) {
	t.Parallel()
	got := ticketAuth(&pve.Ticket{Username: "dev@pve!ci", Token: "dev@pve!ci=secret"}) //nolint:gosec // a made-up one
	if want := []string{"PROXMOX_VE_API_TOKEN=dev@pve!ci=secret"}; !slices.Equal(got, want) {
		t.Errorf("ticketAuth = %v, want %v", got, want)
	}
	got = ticketAuth(&pve.Ticket{Username: "dev@pve", Ticket: "PVE:dev@pve:1", CSRF: "csrf"})
	if want := []string{"PROXMOX_VE_AUTH_TICKET=PVE:dev@pve:1", "PROXMOX_VE_CSRF_PREVENTION_TOKEN=csrf"}; !slices.Equal(got, want) {
		t.Errorf("ticketAuth = %v, want %v", got, want)
	}
}
