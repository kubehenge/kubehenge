package ui

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/release"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

func withUpgrades(t *testing.T, h *harness) *Upgrades {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ix := release.Index{Expires: h.clock.Add(release.Validity)}
	for _, v := range []string{"0.3.0", "0.1.0", "0.2.0", "0.1.4"} {
		ix.Releases = append(ix.Releases, release.Release{Version: v, URL: "https://example.org/" + v, SHA256: strings.Repeat("0", 64)})
	}
	raw, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	sig := release.Sign(raw, priv)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write([]byte(sig))
			return
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	spool := t.TempDir()
	if err := os.MkdirAll(filepath.Join(spool, "requests"), 0o700); err != nil {
		t.Fatal(err)
	}
	up := &Upgrades{
		Spool: appliance.Spool(spool), Index: srv.URL + "/index.json", HTTP: srv.Client(),
		SettingsPath: filepath.Join(t.TempDir(), "upgrades.json"), Now: func() time.Time { return h.clock },
		Current: "0.1.0", Keys: []ed25519.PublicKey{pub},
	}
	h.server.Upgrades = up
	return up
}

func TestUpgradeStatusOfAnotherRelease(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		st   appliance.UpdateStatus
		kept bool
	}{
		{appliance.UpdateStatus{State: "succeeded", From: "0.1.0", To: "0.1.1"}, true},
		// Upgraded on from the shell since.
		{appliance.UpdateStatus{State: "succeeded", From: "0.0.9", To: "0.1.0"}, false},
		{appliance.UpdateStatus{State: "failed", From: "0.1.1"}, true},
		{appliance.UpdateStatus{State: "failed", From: "0.1.0"}, false},
		{appliance.UpdateStatus{State: "current", From: "0.1.1"}, true},
	} {
		if got := aboutRelease(&c.st, "0.1.1"); (got != nil) != c.kept {
			t.Errorf("aboutRelease(%+v) = %v, want kept %v", c.st, got, c.kept)
		}
	}
	if aboutRelease(nil, "0.1.1") != nil {
		t.Error("no status kept as one")
	}
}

func TestApplianceUpgrades(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	up := withUpgrades(t, h)
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("GET", "/api/appliance/upgrades", "")
	expect(t, rec, http.StatusOK)
	var v upgradesView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || strings.Join(v.Available, ",") != "0.1.4,0.2.0,0.3.0" || v.Settings.Policy != release.PolicyOff {
		t.Fatalf("view = %s", rec.Body)
	}

	expect(t, a.do("POST", "/api/appliance/upgrades", `{"version":"0.1.0"}`), http.StatusUnprocessableEntity)
	expect(t, a.do("POST", "/api/appliance/upgrades", `{"version":"0.9.0"}`), http.StatusUnprocessableEntity)
	expect(t, a.do("POST", "/api/appliance/upgrades", `{"version":"0.3.0"}`), http.StatusAccepted)
	req, err := os.ReadFile(filepath.Join(string(up.Spool), "requests", "upgrade.json"))
	if err != nil || string(req) != `{"version":"0.3.0"}` {
		t.Fatalf("request = %s, %v", req, err)
	}

	expect(t, a.do("PUT", "/api/appliance/upgrades/settings", `{"policy":"major","hour":3}`), http.StatusUnprocessableEntity)
	expect(t, a.do("PUT", "/api/appliance/upgrades/settings", `{"policy":"minor","hour":24}`), http.StatusUnprocessableEntity)
	expect(t, a.do("PUT", "/api/appliance/upgrades/settings", `{"policy":"minor","hour":4,"timeZone":"Mars/Olympus"}`), http.StatusUnprocessableEntity)
	expect(t, a.do("PUT", "/api/appliance/upgrades/settings", `{"policy":"minor","hour":4,"timeZone":"Local"}`), http.StatusUnprocessableEntity)
	if rec := a.do("PUT", "/api/appliance/upgrades/settings", `{"policy":"minor","hour":4}`); !strings.Contains(rec.Body.String(), `"timeZone":"UTC"`) {
		t.Fatalf("settings = %s", rec.Body)
	}
	expect(t, a.do("PUT", "/api/appliance/upgrades/settings", `{"policy":"minor","hour":4,"timeZone":"Europe/Berlin"}`), http.StatusOK)

	d := h.browser()
	d.signIn("dev@pve")
	expect(t, d.do("GET", "/api/appliance/upgrades", ""), http.StatusForbidden)
	expect(t, d.do("POST", "/api/appliance/upgrades", `{"version":"0.3.0"}`), http.StatusForbidden)
	expect(t, d.do("PUT", "/api/appliance/upgrades/settings", `{"policy":"off","hour":4}`), http.StatusForbidden)
}

// An index past its expiry offers nothing, not even the copy fetched while it was good.
func TestApplianceUpgradesRefuseAnExpiredIndex(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withUpgrades(t, h)
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("GET", "/api/appliance/upgrades", ""), http.StatusOK)
	h.clock = h.clock.Add(release.Validity)
	a = h.browser()
	a.signIn("admin@pve")
	rec := a.do("GET", "/api/appliance/upgrades", "")
	expect(t, rec, http.StatusOK)
	var v upgradesView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || len(v.Available) != 0 || !strings.Contains(v.Error, "expired") {
		t.Fatalf("view = %s", rec.Body)
	}
	expect(t, a.do("POST", "/api/appliance/upgrades", `{"version":"0.3.0"}`), http.StatusBadGateway)
}

func TestSetupOutsideTheAppliance(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("GET", "/api/setup", "")
	expect(t, rec, http.StatusOK)
	if rec.Body.String() != "{\"appliance\":false,\"ready\":true,\"allowed\":false}\n" {
		t.Fatalf("setup = %s", rec.Body)
	}
	expect(t, a.do("POST", "/api/setup", `{"settings":{},"passphrase":""}`), http.StatusNotFound)
	expect(t, a.do("GET", "/api/appliance/upgrades", ""), http.StatusNotFound)
}

func TestSetupNeedsAnAdministrator(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	d := h.browser()
	d.signIn("dev@pve")
	rec := d.do("GET", "/api/setup", "")
	if rec.Body.String() != "{\"appliance\":true,\"ready\":true,\"allowed\":false}\n" {
		t.Fatalf("setup = %s", rec.Body)
	}
	expect(t, d.do("POST", "/api/setup", `{"settings":{},"passphrase":"correct horse battery staple"}`), http.StatusForbidden)
}

// Restores through the guest agent grant Kubehenge a token with VM.GuestAgent.Unrestricted, which
// only who holds it may hand on.
func TestSetupWithEtcdRestoresNeedsTheGuestAgent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	a := h.browser()
	a.signIn("admin@pve")
	setUp := func(settings string) *httptest.ResponseRecorder {
		return a.do("POST", "/api/setup", `{"settings":`+settings+`,"passphrase":"correct horse battery staple"}`)
	}
	expect(t, setUp(`{"etcdRestore":true}`), http.StatusForbidden)
	h.accounts.mu.Lock()
	h.accounts.grants["admin@pve"]["/"] = append(h.accounts.grants["admin@pve"]["/"], "VM.GuestAgent.Unrestricted")
	h.accounts.mu.Unlock()
	expect(t, setUp(`{"etcdRestore":true}`), http.StatusUnprocessableEntity)
}

// Switching Proxmox VE HA on grants Kubehenge's token Sys.Console on /, and switching it off removes
// the HA objects the controller left, both of which Proxmox VE lets only Sys.Console do.
func TestSetupWithProxmoxHANeedsSysConsole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	a := h.browser()
	a.signIn("admin@pve")
	setUp := func(settings string) *httptest.ResponseRecorder {
		return a.do("POST", "/api/setup", `{"settings":`+settings+`,"passphrase":"correct horse battery staple"}`)
	}
	expect(t, setUp(`{}`), http.StatusUnprocessableEntity)
	expect(t, setUp(`{"proxmoxHA":true}`), http.StatusForbidden)

	saved := filepath.Join(h.server.Appliance.Dir, "management")
	if err := os.MkdirAll(saved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saved, "settings.json"), []byte(`{"proxmoxHA":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	expect(t, setUp(`{}`), http.StatusForbidden)

	h.accounts.mu.Lock()
	h.accounts.grants["admin@pve"]["/"] = append(h.accounts.grants["admin@pve"]["/"], "Sys.Console")
	h.accounts.mu.Unlock()
	expect(t, setUp(`{}`), http.StatusUnprocessableEntity)

	// Once a run is through, the interface switches to the management cluster, and the clusters
	// leave HA only when a run switched it off, until a release has got through.
	var switched int
	h.server.NewKube = func([]byte) (client.Client, error) { switched++; return h.kube, nil }
	admin := &pve.Ticket{Username: "admin@pve"}
	applied := func(t *testing.T, ticket *pve.Ticket) error {
		t.Helper()
		return h.server.applied(ticket)(t.Context(), []byte("kubeconfig"))
	}
	if err := applied(t, admin); err != nil || len(h.accounts.released) != 0 {
		t.Fatalf("with HA on: released %v, %v", h.accounts.released, err)
	}
	for name, content := range map[string]string{"settings.json": `{}`, "ha-release": ""} {
		if err := os.WriteFile(filepath.Join(saved, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := applied(t, &pve.Ticket{Username: "nobody@pve"}); err == nil {
		t.Error("a release Proxmox VE refuses reads as done")
	}
	for range 2 {
		if err := applied(t, admin); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.accounts.released; switched != 4 || len(got) != 1 || got[0] != "Kubehenge cluster kubehenge/" {
		t.Errorf("switched %d times, released %v", switched, got)
	}
}

// root@pam's password is checked by signing in with it alone, as the CSI plugin will, before any
// run starts; a stored one serves while volume snapshots stay on.
func TestSetupChecksRootPassword(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	h.accounts.mu.Lock()
	h.accounts.passwords["root@pam"] = "r00t"
	h.accounts.mu.Unlock()
	a := h.browser()
	a.signIn("admin@pve")
	settings := `{"nodes":["pve1"],"imageDatastore":"local","vmDatastore":"local-lvm","bridge":"vmbr0","nodeAddresses":["10.0.0.11/24"],` +
		`"apiAddress":"10.0.0.10","gateway":"10.0.0.1","dnsServers":["10.0.0.1"],"workloadDatastores":["local-lvm"],"workloadBridges":["vmbr0"],` +
		`"bootstrapDatastore":"local","clusterVolumeSnapshots":true}`
	setUp := func(extra string) *httptest.ResponseRecorder {
		// The passphrase is too short, so that a request past the checks ends there, before a run.
		return a.do("POST", "/api/setup", `{"settings":`+settings+`,"passphrase":"short"`+extra+`}`)
	}
	for extra, want := range map[string]string{ //nolint:gosec // the fake Proxmox VE's passwords
		``:                        appliance.ErrNoRootPassword.Error(),
		`,"rootPassword":"wrong"`: "Proxmox VE refused root@pam's password",
		`,"rootPassword":"r00t"`:  "passphrase",
	} {
		rec := setUp(extra)
		expect(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%q: %s, want %q", extra, rec.Body, want)
		}
	}

	h.accounts.mu.Lock()
	h.accounts.totp["root@pam"] = "123456"
	h.accounts.mu.Unlock()
	if rec := setUp(`,"rootPassword":"r00t"`); !strings.Contains(rec.Body.String(), "second factor") {
		t.Errorf("a root@pam with a second factor: %s", rec.Body)
	}
}

func TestSetupTellsAdministratorsAboutBackups(t *testing.T) {
	t.Parallel()
	for vmid, want := range map[int]string{0: "", 150: `"backup":true`, 151: `"backup":false`} {
		h := newHarness(t)
		h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
		h.server.ApplianceVMID = vmid
		a := h.browser()
		a.signIn("admin@pve")
		rec := a.do("GET", "/api/setup", "")
		if got := rec.Body.String(); want == "" && strings.Contains(got, "backup") || !strings.Contains(got, want) {
			t.Errorf("guest %d: setup = %s", vmid, got)
		}
		// The page asks every few seconds during a run; the backup jobs are read once a while.
		if got := a.do("GET", "/api/setup", "").Body.String(); !strings.Contains(got, want) || vmid != 0 && h.accounts.backupChecks != 1 {
			t.Errorf("guest %d: asked again: setup = %s after %d checks", vmid, got, h.accounts.backupChecks)
		}
		d := h.browser()
		d.signIn("dev@pve")
		if rec := d.do("GET", "/api/setup", ""); strings.Contains(rec.Body.String(), "backup") {
			t.Errorf("guest %d: told someone who may not set up: %s", vmid, rec.Body)
		}
	}
}

func TestSetupTellsAdministratorsWhenTheCertificateRunsOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	h.server.ApplianceVMID = 150
	h.server.CertificateExpires = time.Date(2028, 1, 16, 20, 17, 48, 0, time.UTC)
	a := h.browser()
	a.signIn("admin@pve")
	if got := a.do("GET", "/api/setup", "").Body.String(); !strings.Contains(got, `"certificateExpires":"2028-01-16T20:17:48Z","applianceVMID":150`) {
		t.Errorf("setup = %s", got)
	}
	d := h.browser()
	d.signIn("dev@pve")
	if got := d.do("GET", "/api/setup", "").Body.String(); strings.Contains(got, "certificate") || strings.Contains(got, "applianceVMID") {
		t.Errorf("told someone who may not set up: %s", got)
	}
}

func TestSetupTellsAdministratorsTheAPICertificateGoesUnverified(t *testing.T) {
	t.Parallel()
	for _, insecure := range []bool{false, true} {
		h := newHarness(t)
		h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
		h.server.ProxmoxInsecure = insecure
		a := h.browser()
		a.signIn("admin@pve")
		if got := a.do("GET", "/api/setup", "").Body.String(); strings.Contains(got, `"proxmoxInsecure":true`) != insecure {
			t.Errorf("insecure %v: setup = %s", insecure, got)
		}
		d := h.browser()
		d.signIn("dev@pve")
		if got := d.do("GET", "/api/setup", "").Body.String(); strings.Contains(got, "proxmoxInsecure") {
			t.Errorf("told someone who may not set up: %s", got)
		}
	}
}

func TestRemoveNeedsNoClustersLeft(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("POST", "/api/setup/remove", `{"passphrase":"correct horse battery staple","confirm":"yes"}`), http.StatusUnprocessableEntity)
	rec := a.do("POST", "/api/setup/remove", `{"passphrase":"correct horse battery staple","confirm":"remove"}`)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "delete the 1 clusters first") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	d := h.browser()
	d.signIn("dev@pve")
	expect(t, d.do("POST", "/api/setup/remove", `{"passphrase":"correct horse battery staple","confirm":"remove"}`), http.StatusForbidden)
}

// While a job holds the appliance, setup and removal are refused as conflicts, before the removal
// asks a management cluster that may be going.
func TestSetupAndRemovalWaitForTheRunningJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	withTemplates(t, h)
	built := make(chan struct{})
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) {
		<-built
		return 9010, nil
	}
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("POST", "/api/templates", `{"distribution":"rke2","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`), http.StatusAccepted)
	// The build ends, and records itself in the appliance's folder, before the test's folder goes.
	defer func() {
		close(built)
		deadline := time.Now().Add(5 * time.Second)
		for h.server.Appliance.TemplateJob().State == "running" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	rec := a.do("POST", "/api/setup/remove", `{"passphrase":"correct horse battery staple","confirm":"remove"}`)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), appliance.ErrTemplateBusy.Error()) {
		t.Errorf("removal refused with %s", rec.Body)
	}
	settings := `{"nodes":["pve1"],"imageDatastore":"local","vmDatastore":"local-lvm","bridge":"vmbr0","nodeAddresses":["10.0.0.11/24"],` +
		`"apiAddress":"10.0.0.10","gateway":"10.0.0.1","dnsServers":["10.0.0.1"],"workloadDatastores":["local-lvm"],"workloadBridges":["vmbr0"],"bootstrapDatastore":"local"}`
	rec = a.do("POST", "/api/setup", `{"passphrase":"correct horse battery staple","settings":`+settings+`}`)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), appliance.ErrTemplateBusy.Error()) {
		t.Errorf("setup refused with %s", rec.Body)
	}
}

func TestAutomaticUpgradeIsDueOnceADayAtItsHour(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	up := withUpgrades(t, h)
	h.clock = time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	if _, ok := up.due(false); ok {
		t.Fatal("due with the policy off")
	}
	if err := os.WriteFile(up.SettingsPath, []byte(`{"policy":"patch","hour":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := up.due(true); ok {
		t.Fatal("due during an apply")
	}
	req, ok := up.due(false)
	if !ok || req.Policy != release.PolicyPatch {
		t.Fatalf("not due at its hour: %+v", req)
	}
	h.clock = h.clock.Add(30 * time.Minute)
	if _, ok := up.due(false); ok {
		t.Fatal("due twice in one day")
	}
	// An upgrade restarts the interface; the new one remembers the check.
	restarted := &Upgrades{SettingsPath: up.SettingsPath, Now: up.Now}
	if _, ok := restarted.due(false); ok {
		t.Fatal("due again after a restart")
	}
	h.clock = h.clock.Add(2 * time.Hour)
	if _, ok := up.due(false); ok {
		t.Fatal("due outside its hour")
	}
	h.clock = time.Date(2026, 10, 15, 4, 5, 0, 0, time.UTC)
	if _, ok := up.due(false); !ok {
		t.Fatal("not due the next day")
	}
}

// The interface sleeps until the chosen hour rather than looking every minute of the day.
func TestAutomaticUpgradeSleepsUntilItsHour(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	up := withUpgrades(t, h)
	if _, on := up.untilHour(h.clock, false); on {
		t.Fatal("waiting for an hour with the policy off")
	}
	if wait, on := up.untilHour(h.clock, true); !on || wait != 18*time.Hour {
		t.Fatalf("with the management cluster's automatic upgrades on: %s, %v", wait, on)
	}
	if err := os.WriteFile(up.SettingsPath, []byte(`{"policy":"patch","hour":3,"timeZone":"Europe/Berlin"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for now, want := range map[time.Time]time.Duration{
		time.Date(2026, 10, 14, 1, 30, 0, 0, berlin):   90 * time.Minute,
		time.Date(2026, 10, 14, 3, 20, 0, 0, berlin):   0,
		time.Date(2026, 10, 14, 5, 0, 0, 0, berlin):    22 * time.Hour,
		time.Date(2026, 10, 24, 23, 0, 0, 0, berlin):   5 * time.Hour, // summer time ends at 03:00 on the 25th
		time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC): 2 * time.Hour,
	} {
		if got, on := up.untilHour(now, false); !on || got != want {
			t.Errorf("at %s: %s, want %s", now, got, want)
		}
	}
}

// The hour is the one in the chosen time zone, whatever the appliance's clock says.
func TestAutomaticUpgradeHourIsInItsTimeZone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	up := withUpgrades(t, h)
	if err := os.WriteFile(up.SettingsPath, []byte(`{"policy":"patch","hour":4,"timeZone":"Asia/Tokyo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.clock = time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	if _, ok := up.due(false); ok {
		t.Fatal("due at 04:00 UTC, which is 13:00 in Tokyo")
	}
	h.clock = time.Date(2026, 10, 14, 19, 10, 0, 0, time.UTC)
	if _, ok := up.due(false); !ok {
		t.Fatal("not due at 04:10 in Tokyo")
	}
}
