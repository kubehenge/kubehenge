package ui

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
