package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const week = 7 * 24 * time.Hour

// onAppliance is an appliance with a key volume, whose interface keeps the CLI's sessions.
func onAppliance(t *testing.T) *appliance.Appliance {
	t.Helper()
	return &appliance.Appliance{Dir: t.TempDir(), KeyDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
}

// restart starts h's interface anew on a, as an upgrade or a certificate renewal does, and returns
// what it logs.
func (h *harness) restart(a *appliance.Appliance) *bytes.Buffer {
	h.t.Helper()
	var logged bytes.Buffer
	old := h.server
	h.server = &Server{
		Accounts: old.Accounts, Kube: old.Kube, WorkloadPool: old.WorkloadPool, ClassNamespace: old.ClassNamespace,
		Namespace: old.Namespace, SessionMaxAge: old.SessionMaxAge, CLISessionMaxAge: old.CLISessionMaxAge, Assets: old.Assets,
		Appliance: a, Log: slog.New(slog.NewTextHandler(&logged, nil)),
	}
	h.handler = h.server.Handler()
	h.server.clock = func() time.Time { return h.clock }
	h.server.sessions.now = func() time.Time { return h.clock }
	h.server.limiter.now = func() time.Time { return h.clock }
	return &logged
}

// keptOn is what a keeps of the interface's sessions.
func keptOn(t *testing.T, a *appliance.Appliance) []keptSession {
	t.Helper()
	raw, err := a.KeptSessions()
	if err != nil {
		t.Fatal(err)
	}
	var kept []keptSession
	if raw != nil {
		if err := json.Unmarshal(raw, &kept); err != nil {
			t.Fatal(err)
		}
	}
	return kept
}

// A password sign-in by the CLI lasts a week however active, while a browser's keeps its own time.
func TestACLIPasswordSignInLastsAWeek(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	start := h.clock
	cli := h.browser()
	cli.cli = true
	cli.signIn("admin@pve")
	web := h.browser()
	web.signIn("admin@pve")
	for h.clock.Sub(start) < week-2*time.Hour {
		h.clock = h.clock.Add(time.Hour + time.Minute)
		expect(t, cli.do("GET", "/api/session", ""), http.StatusOK)
		want := http.StatusOK
		if h.clock.Sub(start) >= h.server.SessionMaxAge {
			want = http.StatusUnauthorized
		}
		expect(t, web.do("GET", "/api/session", ""), want)
	}
	h.clock = start.Add(week)
	expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// A token sign-in keeps a browser's time and is never kept, whoever makes it.
func TestATokenSignInFromTheCLIKeepsTheBrowsersRules(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := onAppliance(t)
	h.restart(a)
	start := h.clock
	b := h.browser()
	b.cli = true
	expect(t, b.do("POST", "/api/session/token", tokenSignIn), http.StatusOK)
	if kept := keptOn(t, a); len(kept) != 0 {
		t.Fatalf("a token session is kept: %d", len(kept))
	}
	for h.clock.Sub(start) < h.server.SessionMaxAge-time.Hour {
		h.clock = h.clock.Add(time.Hour + time.Minute)
		expect(t, b.do("GET", "/api/session", ""), http.StatusOK)
	}
	h.clock = start.Add(h.server.SessionMaxAge)
	expect(t, b.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// The CLI's sessions outlive a restart, sealed in a file of the appliance's user alone, while a
// browser's end.
func TestCLISessionsOutliveARestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := onAppliance(t)
	h.restart(a)
	cli := h.browser()
	cli.cli = true
	cli.signIn("admin@pve")
	web := h.browser()
	web.signIn("dev@pve")

	file := filepath.Join(a.Dir, "sessions.sealed")
	raw, err := os.ReadFile(file) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ticket:admin@pve", cli.csrf, cli.cookie, "admin@pve"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("the kept sessions hold %q in the clear", secret)
		}
	}
	if info, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the kept sessions: %v, %v", info, err)
	}

	logged := h.restart(a)
	expect(t, cli.do("GET", "/api/session", ""), http.StatusOK)
	expect(t, web.do("GET", "/api/session", ""), http.StatusUnauthorized)
	if strings.Contains(logged.String(), "ticket:") || strings.Contains(logged.String(), cli.csrf) {
		t.Errorf("the log holds a secret: %s", logged)
	}

	// The CSRF token comes back with the session and is still asked for.
	csrf := cli.csrf
	cli.csrf = "guess"
	expect(t, cli.do("DELETE", "/api/session", ""), http.StatusForbidden)
	cli.csrf = csrf
	expect(t, cli.do("DELETE", "/api/session", ""), http.StatusNoContent)
}

// What no longer opens or parses is discarded with one line, and the interface starts without it.
func TestUnreadableKeptSessionsAreDiscarded(t *testing.T) {
	t.Parallel()
	for name, spoil := range map[string]func(a *appliance.Appliance) error{
		"damaged": func(a *appliance.Appliance) error {
			return os.WriteFile(filepath.Join(a.Dir, "sessions.sealed"), []byte("not sealed"), 0o600)
		},
		"key lost": func(a *appliance.Appliance) error { return os.Remove(filepath.Join(a.KeyDir, "sessions.key")) },
		"not JSON": func(a *appliance.Appliance) error { return a.KeepSessions([]byte("{")) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			a := onAppliance(t)
			h.restart(a)
			cli := h.browser()
			cli.cli = true
			cli.signIn("admin@pve")
			if err := spoil(a); err != nil {
				t.Fatal(err)
			}
			logged := h.restart(a)
			if lines := strings.Count(logged.String(), "\n"); lines != 1 || !strings.Contains(logged.String(), "the kept sessions are discarded") {
				t.Errorf("logged %d lines: %s", lines, logged)
			}
			expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
			if kept := keptOn(t, a); len(kept) != 0 {
				t.Errorf("%d sessions still kept", len(kept))
			}
			cli.signIn("admin@pve")
			if kept := keptOn(t, a); len(kept) != 1 {
				t.Errorf("%d sessions kept after a new sign-in", len(kept))
			}
		})
	}
}

// After a restart a kept session works only while its ticket is valid or renews. The renewed ticket
// is what the next restart reads.
func TestAKeptSessionNeedsALiveTicket(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := onAppliance(t)
	h.restart(a)
	start := h.clock
	cli := h.browser()
	cli.cli = true
	cli.signIn("admin@pve")

	h.clock = start.Add(renewAfter + time.Minute)
	h.restart(a)
	expect(t, cli.do("GET", "/api/session", ""), http.StatusOK)
	// Past the first ticket's life, the renewed one holds.
	h.clock = start.Add(pve.TicketLifetime + time.Minute)
	h.restart(a)
	rec := cli.do("GET", "/api/session", "")
	expect(t, rec, http.StatusOK)

	// A ticket that ran out while the interface was down ends the session at its first use. The
	// answer carries no version, which tells the CLI to log in again.
	h.clock = h.clock.Add(pve.TicketLifetime)
	h.restart(a)
	rec = cli.do("GET", "/api/session", "")
	expect(t, rec, http.StatusUnauthorized)
	if rec.Header().Get(versionHeader) != "" {
		t.Error("an ended session's answer carries the version")
	}
	if kept := keptOn(t, a); len(kept) != 0 {
		t.Errorf("%d ended sessions still kept", len(kept))
	}

	// One whose renewal Proxmox VE refuses ends too.
	cli.signIn("admin@pve")
	h.clock = h.clock.Add(renewAfter + time.Minute)
	h.restart(a)
	h.accounts.mu.Lock()
	delete(h.accounts.passwords, "admin@pve")
	h.accounts.mu.Unlock()
	expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
	if kept := keptOn(t, a); len(kept) != 0 {
		t.Errorf("%d refused sessions still kept", len(kept))
	}
}

// Signing out takes a kept session off the disk at once, and so does pruning an ended one.
func TestEndedSessionsLeaveTheDisk(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := onAppliance(t)
	h.restart(a)
	cli := h.browser()
	cli.cli = true
	cli.signIn("admin@pve")
	if kept := keptOn(t, a); len(kept) != 1 {
		t.Fatalf("%d sessions kept", len(kept))
	}
	expect(t, cli.do("DELETE", "/api/session", ""), http.StatusNoContent)
	if kept := keptOn(t, a); len(kept) != 0 {
		t.Fatalf("%d sessions kept after signing out", len(kept))
	}

	cli.signIn("admin@pve")
	h.clock = h.clock.Add(pve.TicketLifetime)
	h.server.sessions.prune()
	if kept := keptOn(t, a); len(kept) != 0 {
		t.Fatalf("%d sessions kept after pruning", len(kept))
	}
}

// tend runs one round of what the interface does every minute.
func (h *harness) tend() {
	h.t.Helper()
	h.server.sessions.tend(h.t.Context())
}

// held counts the sessions in memory.
func (h *harness) held() int {
	s := h.server.sessions
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// cliOnAppliance signs the CLI in to an interface that keeps its sessions on a.
func cliOnAppliance(t *testing.T) (*harness, *appliance.Appliance, *browser) {
	t.Helper()
	h := newHarness(t)
	a := onAppliance(t)
	h.restart(a)
	cli := h.browser()
	cli.cli = true
	cli.signIn("admin@pve")
	return h, a, cli
}

// keptIssued is when the ticket of the one session a keeps was issued.
func keptIssued(t *testing.T, a *appliance.Appliance) time.Time {
	t.Helper()
	kept := keptOn(t, a)
	if len(kept) != 1 {
		t.Fatalf("%d sessions kept, want 1", len(kept))
	}
	return kept[0].Issued
}

// unansweredRenewals counts the renewals and fails them with err while it is set, as a Proxmox VE
// that cannot be reached.
type unansweredRenewals struct {
	Accounts

	mu    sync.Mutex
	err   error
	calls int
}

func (f *unansweredRenewals) Renew(ctx context.Context, t *pve.Ticket) (*pve.Ticket, error) {
	f.mu.Lock()
	f.calls++
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Accounts.Renew(ctx, t)
}

func (f *unansweredRenewals) answer(err error) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	return f.calls
}

// A CLI session nobody uses has its ticket renewed before it runs out, and the renewed ticket is
// what is kept.
func TestAnUnusedCLISessionIsRenewed(t *testing.T) {
	t.Parallel()
	h, a, cli := cliOnAppliance(t)
	for range 5 {
		h.clock = h.clock.Add(renewAfter + time.Minute)
		h.tend()
		if issued := keptIssued(t, a); !issued.Equal(h.clock) {
			t.Fatalf("the kept ticket is of %s, want %s", issued, h.clock)
		}
	}
	expect(t, cli.do("GET", "/api/session", ""), http.StatusOK)
}

// After a restart a kept session is renewed when its ticket is as old as before the restart. One
// whose ticket ran out while the interface was down ends without a renewal.
func TestKeptSessionsAreRenewedAfterARestart(t *testing.T) {
	t.Parallel()
	h, a, cli := cliOnAppliance(t)
	renewals := func() int {
		h.accounts.mu.Lock()
		defer h.accounts.mu.Unlock()
		return h.accounts.renewals
	}
	h.clock = h.clock.Add(renewAfter - time.Minute)
	h.restart(a)
	h.tend()
	if n := renewals(); n != 0 {
		t.Fatalf("%d renewals of a young ticket", n)
	}
	h.clock = h.clock.Add(time.Minute)
	h.tend()
	if issued := keptIssued(t, a); renewals() != 1 || !issued.Equal(h.clock) {
		t.Fatalf("%d renewals, the kept ticket is of %s", renewals(), issued)
	}

	h.clock = h.clock.Add(pve.TicketLifetime)
	h.restart(a)
	h.tend()
	if n := renewals(); n != 1 {
		t.Errorf("%d renewals, a ticket that ran out was renewed", n)
	}
	if kept := keptOn(t, a); len(kept) != 0 || h.held() != 0 {
		t.Errorf("%d kept, %d held after the ticket ran out", len(kept), h.held())
	}
	expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// A renewal Proxmox VE refuses ends the session at once, in memory and on the disk.
func TestARefusedRenewalEndsAnUnusedCLISession(t *testing.T) {
	t.Parallel()
	h, a, cli := cliOnAppliance(t)
	h.clock = h.clock.Add(renewAfter + time.Minute)
	h.accounts.mu.Lock()
	delete(h.accounts.passwords, "admin@pve")
	h.accounts.mu.Unlock()
	h.tend()
	if kept := keptOn(t, a); len(kept) != 0 || h.held() != 0 {
		t.Fatalf("%d kept, %d held after the refusal", len(kept), h.held())
	}
	expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// A renewal Proxmox VE does not answer keeps the session and is tried again every round, until the
// ticket runs out.
func TestAnUnansweredRenewalIsTriedAgain(t *testing.T) {
	t.Parallel()
	h, a, cli := cliOnAppliance(t)
	down := &unansweredRenewals{Accounts: h.accounts, err: errors.New("no route to host")}
	h.server.sessions.accounts = down
	issued := h.clock
	h.clock = h.clock.Add(renewAfter + time.Minute)
	h.tend()
	h.clock = h.clock.Add(time.Minute)
	h.tend()
	if calls, kept := down.answer(nil), keptIssued(t, a); calls != 2 || !kept.Equal(issued) {
		t.Fatalf("%d renewals tried, the kept ticket is of %s", calls, kept)
	}
	h.clock = h.clock.Add(time.Minute)
	h.tend()
	if kept := keptIssued(t, a); !kept.Equal(h.clock) {
		t.Fatalf("the kept ticket is of %s once Proxmox VE answers, want %s", kept, h.clock)
	}

	down.answer(errors.New("no route to host"))
	h.clock = h.clock.Add(pve.TicketLifetime - time.Minute)
	h.tend()
	keptIssued(t, a)
	h.clock = h.clock.Add(time.Minute)
	h.tend()
	if kept := keptOn(t, a); len(kept) != 0 || h.held() != 0 {
		t.Fatalf("%d kept, %d held after the ticket ran out", len(kept), h.held())
	}
	expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// Only the CLI's password sessions are renewed unused. A browser's, an OpenID and a token session
// run out with their ticket.
func TestOtherSessionsAreNotRenewedUnused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.PublicURL = "https://kubehenge.example"
	counted := &unansweredRenewals{Accounts: h.accounts}
	h.server.sessions.accounts = counted
	web := h.browser()
	web.signIn("admin@pve")
	token := h.browser()
	token.cli = true
	expect(t, token.do("POST", "/api/session/token", tokenSignIn), http.StatusOK)
	rec := get(t, h, "/api/openid/start?realm=sso")
	rec = get(t, h, "/api/openid/callback?code=good&state="+url.QueryEscape(`"st-sso"`), cookie(rec, openIDCookie))
	openID := h.browser()
	if c := cookie(rec, sessionCookie); c != nil {
		openID.cookie = c.Value
	}
	for _, b := range []*browser{web, token, openID} {
		expect(t, b.do("GET", "/api/session", ""), http.StatusOK)
	}

	h.clock = h.clock.Add(renewAfter + time.Minute)
	h.tend()
	if calls := counted.answer(nil); calls != 0 || h.held() != 3 {
		t.Fatalf("%d renewals, %d sessions held", calls, h.held())
	}
	h.clock = h.clock.Add(renewAfter)
	h.tend()
	if calls := counted.answer(nil); calls != 0 || h.held() != 0 {
		t.Fatalf("%d renewals, %d sessions held after the tickets ran out", calls, h.held())
	}
}

// The demo's session is not renewed unused either.
func TestTheDemosSessionIsNotRenewedUnused(t *testing.T) {
	t.Parallel()
	s, b := newDemo(t)
	b.signIn()
	counted := &unansweredRenewals{Accounts: s.sessions.accounts}
	s.sessions.accounts = counted
	// The demo's ticket is of the machine's own time.
	later := time.Now().Add(renewAfter + time.Minute)
	s.sessions.now = func() time.Time { return later }
	s.sessions.tend(t.Context())
	if calls := counted.answer(nil); calls != 0 {
		t.Fatalf("%d renewals", calls)
	}
}

// Renewals never carry a CLI session past its week.
func TestAnUnusedCLISessionEndsAfterAWeek(t *testing.T) {
	t.Parallel()
	h, a, cli := cliOnAppliance(t)
	start := h.clock
	for h.clock.Sub(start) < week-renewAfter {
		h.clock = h.clock.Add(renewAfter)
		h.tend()
	}
	keptIssued(t, a)
	h.clock = start.Add(week)
	h.tend()
	if kept := keptOn(t, a); len(kept) != 0 || h.held() != 0 {
		t.Fatalf("%d kept, %d held after a week", len(kept), h.held())
	}
	expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// A sign-out while the ticket is renewed unused leaves the session ended, in memory and on the disk.
func TestSignOutDuringAnUnusedRenewal(t *testing.T) {
	t.Parallel()
	h, a, cli := cliOnAppliance(t)
	slow := &slowRenewals{fakeAccounts: h.accounts, started: make(chan struct{}, 1), release: make(chan struct{})}
	h.server.sessions.accounts = slow
	h.clock = h.clock.Add(renewAfter + time.Minute)
	tended := make(chan struct{})
	go func() {
		defer close(tended)
		h.server.sessions.tend(t.Context())
	}()
	<-slow.started
	expect(t, cli.do("DELETE", "/api/session", ""), http.StatusNoContent)
	close(slow.release)
	<-tended
	if kept := keptOn(t, a); len(kept) != 0 || h.held() != 0 {
		t.Fatalf("%d kept, %d held after signing out", len(kept), h.held())
	}
	expect(t, cli.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// The interface's loop renews unused CLI sessions and returns once its context ends.
func TestTheSessionLoopRenewsAndStops(t *testing.T) {
	t.Parallel()
	h, a, _ := cliOnAppliance(t)
	slow := &slowRenewals{fakeAccounts: h.accounts, started: make(chan struct{}, 1), release: make(chan struct{})}
	h.server.sessions.accounts = slow
	h.clock = h.clock.Add(renewAfter + time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.server.sessions.pruneEvery(ctx, time.Millisecond)
	}()
	<-slow.started
	cancel()
	close(slow.release)
	<-stopped
	if issued := keptIssued(t, a); !issued.Equal(h.clock) {
		t.Fatalf("the kept ticket is of %s, want %s", issued, h.clock)
	}
}

// A sign-in waiting for its second factor is never kept. The session the code starts is the CLI's.
func TestAPendingSecondFactorIsNeverKept(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := onAppliance(t)
	h.restart(a)
	cli := h.browser()
	cli.cli = true
	expect(t, cli.do("POST", "/api/session", `{"username":"otp@pve","password":"secret"}`), http.StatusOK)
	if kept := keptOn(t, a); len(kept) != 0 {
		t.Fatalf("a pending second factor is kept: %d", len(kept))
	}
	h.restart(a)
	expect(t, cli.do("POST", "/api/session/second-factor", `{"kind":"totp","code":"123456"}`), http.StatusUnauthorized)

	expect(t, cli.do("POST", "/api/session", `{"username":"otp@pve","password":"secret"}`), http.StatusOK)
	expect(t, cli.do("POST", "/api/session/second-factor", `{"kind":"totp","code":"123456"}`), http.StatusOK)
	if kept := keptOn(t, a); len(kept) != 1 || kept[0].ID != cli.cookie || kept[0].User != "otp@pve" {
		t.Fatalf("kept after the second factor: %+v", kept)
	}
}
