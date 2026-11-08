package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// refusingAccounts is Proxmox VE answering with the errors a test sets, and as fakeAccounts
// otherwise.
type refusingAccounts struct {
	*fakeAccounts

	signIn      error
	answer      error
	poolMembers error
	reboot      error
	recovery    []int
	signIns     atomic.Int32
}

func (f *refusingAccounts) SignIn(ctx context.Context, username, password string) (*pve.Ticket, *pve.Challenge, error) {
	f.signIns.Add(1)
	if f.signIn != nil {
		return nil, nil, f.signIn
	}
	ticket, challenge, err := f.fakeAccounts.SignIn(ctx, username, password)
	if challenge != nil {
		challenge.Recovery = f.recovery
	}
	return ticket, challenge, err
}

func (f *refusingAccounts) Answer(ctx context.Context, c *pve.Challenge, kind, code string) (*pve.Ticket, error) {
	if f.answer != nil {
		return nil, f.answer
	}
	return f.fakeAccounts.Answer(ctx, c, kind, code)
}

func (f *refusingAccounts) PoolMembers(ctx context.Context, t *pve.Ticket, pool string) ([]pve.Member, error) {
	if f.poolMembers != nil {
		return nil, f.poolMembers
	}
	return f.fakeAccounts.PoolMembers(ctx, t, pool)
}

func (f *refusingAccounts) RebootVM(ctx context.Context, t *pve.Ticket, node string, vmid int) error {
	if f.reboot != nil {
		return f.reboot
	}
	return f.fakeAccounts.RebootVM(ctx, t, node, vmid)
}

func refusing(h *harness) *refusingAccounts {
	f := &refusingAccounts{fakeAccounts: h.accounts}
	h.server.Accounts = f
	return f
}

// errorText is the message of an error answer, without its request ID.
func errorText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Error
}

func apiRefusal(status string, errs map[string]string) error {
	return &pve.APIError{Method: "POST", Path: "/nodes/pve1/qemu/202/status/reboot", Status: status, Errors: errs}
}

const rebootPath = "/api/clusters/default/one/machines/one-md-0-a/reboot"

func TestAnsweredRefusalsKeepProxmoxVEsReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"a locked guest", apiRefusal("500 VM is locked (backup)", nil), "Proxmox VE refused: VM is locked (backup)"},
		{"no quorum", apiRefusal("500 cluster not ready - no quorum?", nil), "Proxmox VE refused: cluster not ready - no quorum?"},
		{"a node pveproxy cannot reach", apiRefusal("595 No route to host pve2 (/etc/pve/nodes/pve2)", nil), "Proxmox VE refused: No route to host pve2\""},
		{"a refused parameter", apiRefusal("400 Parameter verification failed.", map[string]string{"vmid": "value 'x' is not an integer"}), "Proxmox VE refused: Parameter verification failed: vmid value 'x' is not an integer\""},
		{"a path in the reason", apiRefusal("500 unable to open file '/etc/pve/nodes/pve1/qemu-server/202.conf.tmp.1' - Permission denied", nil), "Proxmox VE refused: unable to open file - Permission denied\""},
		{"no reason", apiRefusal("500 ", nil), "Proxmox VE refused: status 500\""},
		{"a long reason", apiRefusal("500 "+strings.Repeat("slow ", 60), nil), "…\""},
		{"a gateway in front", apiRefusal("502 Bad Gateway", nil), "Proxmox VE did not answer\""},
		{"no connection", errors.New("asking proxmox VE: dial tcp 192.0.2.5:8006: connect: connection refused"), "Proxmox VE did not answer\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _ := healthHarness(t)
			refusing(h).reboot = tt.err
			b := h.browser()
			b.signIn("admin@pve")
			rec := b.do("POST", rebootPath, "")
			expect(t, rec, http.StatusBadGateway)
			if body := rec.Body.String(); !strings.Contains(body, tt.want) {
				t.Errorf("answered %s, want it to hold %q", body, tt.want)
			}
		})
	}
}

func TestSignInTellsAStrangerNothingOfProxmoxVEsReasons(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	f := refusing(h)
	wrong := h.browser().do("POST", "/api/session", `{"username":"admin@pve","password":"wrong"}`)
	expect(t, wrong, http.StatusUnauthorized)
	unknown := h.browser().do("POST", "/api/session", `{"username":"nobody@pve","password":"wrong"}`)
	expect(t, unknown, http.StatusUnauthorized)
	if errorText(t, wrong) != errorText(t, unknown) {
		t.Errorf("a wrong password reads %s but an unknown user %s", wrong.Body, unknown.Body)
	}

	f.signIn = apiRefusal("400 Parameter verification failed.", map[string]string{"username": "value does not match"})
	refused := h.browser().do("POST", "/api/session", `{"username":"admin@pve","password":"wrong"}`)
	expect(t, refused, http.StatusUnauthorized)
	if errorText(t, refused) != errorText(t, wrong) {
		t.Errorf("a 400 reads %s, a wrong password %s", refused.Body, wrong.Body)
	}

	f.signIn = apiRefusal("500 pmxcfs is broken", nil)
	down := h.browser().do("POST", "/api/session", `{"username":"admin@pve","password":"secret"}`)
	expect(t, down, http.StatusBadGateway)
	if strings.Contains(down.Body.String(), "pmxcfs") {
		t.Errorf("a stranger reads Proxmox VE's reason: %s", down.Body)
	}
}

func TestLongSignInNamesAreRefusedBeforeProxmoxVE(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	f := refusing(h)
	b := h.browser()
	long := strings.Repeat("a", maxUsernameLength-len("@pve")+1) + "@pve"
	rec := b.do("POST", "/api/session", `{"username":"`+long+`","password":"secret"}`)
	expect(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "at most 64 characters") {
		t.Errorf("answered %s", rec.Body)
	}
	rec = b.do("POST", "/api/session", `{"username":"`+strings.Repeat("a", maxUsernameLength)+`","realm":"pve","password":"secret"}`)
	expect(t, rec, http.StatusBadRequest)
	if f.signIns.Load() != 0 {
		t.Errorf("Proxmox VE was asked %d times about names it refuses", f.signIns.Load())
	}
	exact := strings.Repeat("a", maxUsernameLength-len("@pve")) + "@pve"
	expect(t, b.do("POST", "/api/session", `{"username":"`+exact+`","password":"secret"}`), http.StatusUnauthorized)
	if f.signIns.Load() != 1 {
		t.Error("a name of exactly the limit was not passed on")
	}
}

func TestRefusedSecondFactorsSayWhatTheyCanMean(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		kind     string
		recovery []int
		want     string
	}{
		{"a code, recovery keys left", "totp", []int{0, 1}, "A recovery key unlocks a locked factor."},
		{"a code, recovery keys left, no quorum", "totp", []int{0, 1}, "Without quorum, use a node's console."},
		{"a code, no recovery keys", "totp", nil, "An administrator resets a locked factor."},
		{"a code, no recovery keys, no quorum", "totp", nil, "Without quorum, use a node's console."},
		{"a recovery key", "recovery", []int{0}, "the cluster has no quorum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			f := refusing(h)
			f.recovery = tt.recovery
			f.answer = pve.ErrAuthentication
			b := h.browser()
			expect(t, b.do("POST", "/api/session", `{"username":"otp@pve","password":"secret"}`), http.StatusOK)
			rec := b.do("POST", "/api/session/second-factor", `{"kind":"`+tt.kind+`","code":"000000"}`)
			expect(t, rec, http.StatusUnauthorized)
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("answered %s, want %q", rec.Body, tt.want)
			}
			// Proxmox VE hides the cause, so every refusal counts: a locked factor and a guess look alike.
			for range failuresAllowed - 1 {
				expect(t, b.do("POST", "/api/session/second-factor", `{"kind":"`+tt.kind+`","code":"000000"}`), http.StatusUnauthorized)
			}
			expect(t, b.do("POST", "/api/session/second-factor", `{"kind":"`+tt.kind+`","code":"000000"}`), http.StatusTooManyRequests)
		})
	}
}

func TestAPoolTheUserCannotListNamesPoolAudit(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	refusing(h).poolMembers = pve.ErrForbidden
	b := h.browser()
	b.signIn("admin@pve")

	rec := b.do("POST", rebootPath, "")
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "admin@pve needs Pool.Audit on /pool/kubehenge/") {
		t.Errorf("reboot answered %s", rec.Body)
	}

	view, _ := readHealthView(t, b)
	if !strings.Contains(view.VMs, "needs Pool.Audit on /pool/kubehenge/") {
		t.Errorf("the health page says %q", view.VMs)
	}
}

func TestAPoolListingThatFailsIsNotTakenForMissingPrivileges(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	refusing(h).poolMembers = apiRefusal("500 cluster not ready - no quorum?", nil)
	b := h.browser()
	b.signIn("admin@pve")
	rec := b.do("POST", rebootPath, "")
	expect(t, rec, http.StatusBadGateway)
	if !strings.Contains(rec.Body.String(), "Proxmox VE refused: cluster not ready - no quorum?") {
		t.Errorf("reboot answered %s", rec.Body)
	}
}
