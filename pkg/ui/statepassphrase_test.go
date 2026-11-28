package ui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

// requireStatePassphrase makes the test's passphrase required, so that a wrong one is refused.
func requireStatePassphrase(t *testing.T, a *appliance.Appliance) {
	t.Helper()
	if err := a.RequirePassphrase(t.Context(), statePassphrase, statePassphrase); err != nil {
		t.Fatal(err)
	}
}

// The appliance keeps the state's key: repair access needs the privileges alone. An administrator
// may require a passphrase, which every action then asks for, and switch it off again.
func TestRequiringAStatePassphrase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withManagementCluster(t, h)
	const required = "a passphrase only the admin knows"
	kubeconfig := `{"name":"kubeconfig"}`
	b := h.browser()
	b.signIn("admin@pve")
	d := h.browser()
	d.signIn("dev@pve")
	expect(t, b.do("POST", "/api/setup/credential", kubeconfig), http.StatusOK)
	expect(t, d.do("POST", "/api/setup/credential", kubeconfig), http.StatusForbidden)

	expect(t, d.do("PUT", "/api/setup/state-passphrase", `{"required":true,"newPassphrase":"`+required+`"}`), http.StatusForbidden)
	expect(t, b.do("PUT", "/api/setup/state-passphrase", `{"required":true,"newPassphrase":"short"}`), http.StatusUnprocessableEntity)
	rec := b.do("PUT", "/api/setup/state-passphrase", `{"required":true,"newPassphrase":"`+required+`"}`)
	expect(t, rec, http.StatusOK)
	if got := rec.Body.String(); !strings.Contains(got, `"required":true`) {
		t.Fatalf("switched on = %s", got)
	}
	if got := b.do("GET", "/api/setup", "").Body.String(); !strings.Contains(got, `"passphrase":true`) || !strings.Contains(got, `"passphraseRequired":true`) {
		t.Fatalf("setup once required = %s", got)
	}
	expect(t, b.do("POST", "/api/setup/credential", kubeconfig), http.StatusUnprocessableEntity)
	expect(t, b.do("POST", "/api/setup/credential", `{"name":"kubeconfig","passphrase":"not the passphrase of this state"}`), http.StatusForbidden)
	expect(t, b.do("POST", "/api/setup/credential", `{"name":"kubeconfig","passphrase":"`+required+`"}`), http.StatusOK)

	expect(t, b.do("PUT", "/api/setup/state-passphrase", `{"required":false,"passphrase":"not the passphrase of this state"}`), http.StatusForbidden)
	expect(t, b.do("PUT", "/api/setup/state-passphrase", `{"required":false,"passphrase":"`+required+`"}`), http.StatusOK)
	if got := b.do("GET", "/api/setup", "").Body.String(); strings.Contains(got, `"passphrase":true`) {
		t.Fatalf("setup once switched off = %s", got)
	}
	expect(t, b.do("POST", "/api/setup/credential", kubeconfig), http.StatusOK)
}
