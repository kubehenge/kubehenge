package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// fakeTofu is the appliance package's stand-in for OpenTofu, built once for these tests: the state
// opens only with the passphrase below.
var fakeTofu string

const statePassphrase = "correct horse battery staple"

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "kubehenge-ui")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	fakeTofu = filepath.Join(dir, "tofu")
	if runtime.GOOS == "windows" {
		fakeTofu += ".exe"
	}
	build := exec.CommandContext(context.Background(), "go", "build", "-o", fakeTofu, "../appliance/testdata/tofu") //nolint:gosec // into the tests' own temporary directory
	// The race detector's flags would make the fake as slow to start as the test binary.
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the fake tofu: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func moduleVersions(t *testing.T, module, talos string) {
	t.Helper()
	tf := "locals {\n  tested_versions = {\n    talos = \"" + talos + "\"\n    kubernetes = \"v1.37.1\"\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(module, "versions.tf"), []byte(tf), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withManagementCluster gives the harness an appliance whose management cluster 0.1.0 applied,
// with Talos v1.14.4.
func withManagementCluster(t *testing.T, h *harness) *appliance.Appliance {
	t.Helper()
	module := filepath.Join(t.TempDir(), "management-cluster")
	if err := os.Mkdir(module, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, ".terraform.lock.hcl"), []byte("provider \"registry.opentofu.org/bpg/proxmox\" {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moduleVersions(t, module, "v1.14.4")
	a := &appliance.Appliance{
		Dir: t.TempDir(), KeyDir: t.TempDir(), Module: module, Tofu: fakeTofu, Release: "0.1.0",
		ProxmoxURL: "https://pve.example:8006", ProxmoxCA: "-----BEGIN CERTIFICATE-----\n", Log: slog.New(slog.DiscardHandler),
		InterfaceKubeconfig: func(_ context.Context, admin []byte, _, _ string) ([]byte, error) { return admin, nil },
	}
	s := &appliance.Settings{
		Nodes: []string{"pve1"}, ImageDatastore: "local", VMDatastore: "local-lvm", Bridge: "vmbr0",
		NodeAddresses: []string{"10.0.0.11/24"}, APIAddress: "10.0.0.10", Gateway: "10.0.0.1",
		DNSServers: []string{"10.0.0.1"}, WorkloadDatastores: []string{"local-lvm"}, WorkloadBridges: []string{"vmbr0"},
		BootstrapDatastore: "local",
	}
	ticket := &pve.Ticket{Username: "admin@pve", Ticket: "PVE:admin@pve:1", CSRF: "csrf", Issued: time.Now()}
	if err := a.Apply(s, statePassphrase, ticket, func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := jobEnded(t, a); j.State != "succeeded" {
		t.Fatalf("the first apply: %+v", j)
	}
	// The fake tofu writes no state. The real one does.
	if err := os.WriteFile(filepath.Join(a.Dir, "management", "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.Appliance = a
	h.server.NewKube = func([]byte) (client.Client, error) { return h.kube, nil }
	return a
}

func jobEnded(t *testing.T, a *appliance.Appliance) *appliance.JobView {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if j := a.Current(); j != nil && j.State != "running" {
			return j
		}
	}
	t.Fatal("the job did not end")
	return nil
}

// upgraded has the appliance run a newer release, which carries Talos v1.15.0.
func upgraded(t *testing.T, a *appliance.Appliance, release string) {
	t.Helper()
	a.Release = release
	moduleVersions(t, a.Module, "v1.15.0")
}

func setupUpgrade(t *testing.T, b *browser) *managementUpgrade {
	t.Helper()
	rec := b.do("GET", "/api/setup", "")
	expect(t, rec, http.StatusOK)
	var v setupView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.Upgrade
}

func TestManagementClusterBehindTheAppliance(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withUpgrades(t, h)
	a := withManagementCluster(t, h)
	b := h.browser()
	b.signIn("admin@pve")
	if got := setupUpgrade(t, b); got == nil || got.Behind || got.Applied != "0.1.0" || got.Carried != "0.1.0" || got.Changes != nil {
		t.Fatalf("before an upgrade: %+v", got)
	}

	upgraded(t, a, "0.2.0")
	got := setupUpgrade(t, b)
	want := []versionChange{{Component: "talos", From: "v1.14.4", To: "v1.15.0"}}
	if got == nil || !got.Behind || got.Applied != "0.1.0" || got.Carried != "0.2.0" || !reflect.DeepEqual(got.Changes, want) {
		t.Fatalf("after an upgrade: %+v", got)
	}

	// Those who may not set it up see nothing of it.
	d := h.browser()
	d.signIn("dev@pve")
	if got := setupUpgrade(t, d); got != nil {
		t.Fatalf("seen by a user without privileges: %+v", got)
	}
}

func TestNewerRelease(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		applied, carried string
		newer            bool
	}{
		{"0.1.0", "0.1.1", true},
		{"0.1.9", "0.2.0", true},
		{"0.9.0", "1.0.0", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.1.9", false},
		{"dev", "0.2.0", false},
		{"0.2.0", "dev", false},
		{"", "0.2.0", false},
	} {
		if got := newerRelease(c.applied, c.carried); got != c.newer {
			t.Errorf("newerRelease(%q, %q) = %v", c.applied, c.carried, got)
		}
	}
}

func TestVersionChangesKeepTheModulesOrder(t *testing.T) {
	t.Parallel()
	from := map[string]string{"kubehenge": "0.14.0", "talos": "v1.14.4", "capmox": "v0.9.1", "gone": "v1"}
	to := map[string]string{"kubehenge": "0.15.0", "talos": "v1.14.4", "capmox": "v0.10.0", "new": "v2"}
	want := []versionChange{
		{Component: "capmox", From: "v0.9.1", To: "v0.10.0"},
		{Component: "kubehenge", From: "0.14.0", To: "0.15.0"},
		{Component: "gone", From: "v1"},
		{Component: "new", To: "v2"},
	}
	if got := versionChanges(from, to); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v", got)
	}
}

func managementView(t *testing.T, b *browser) *managementAutomatic {
	t.Helper()
	rec := b.do("GET", "/api/appliance/upgrades", "")
	expect(t, rec, http.StatusOK)
	var v upgradesView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.Management
}

// Switching automatic upgrades of the management cluster on keeps the passphrase and a token of a
// user of its own. Switching them off deletes both.
func TestManagementAutomaticUpgradesOptIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withUpgrades(t, h)
	a := withManagementCluster(t, h)
	b := h.browser()
	b.signIn("admin@pve")
	if got := managementView(t, b); got == nil || !got.Possible || got.On || got.User != upgraderUser {
		t.Fatalf("before switching on: %+v", got)
	}
	switchOn := func(b *browser, passphrase string) int {
		return b.do("PUT", "/api/appliance/upgrades/management", `{"passphrase":"`+passphrase+`"}`).Code
	}
	d := h.browser()
	d.signIn("dev@pve")
	if got := switchOn(d, statePassphrase); got != http.StatusForbidden {
		t.Fatalf("switched on by a user without privileges: %d", got)
	}
	// The token holds the Administrator role, Sys.Console with it.
	if got := switchOn(b, statePassphrase); got != http.StatusForbidden {
		t.Fatalf("switched on without Sys.Console: %d", got)
	}
	h.accounts.mu.Lock()
	h.accounts.grants["admin@pve"]["/"] = append(h.accounts.grants["admin@pve"]["/"], "Sys.Console")
	h.accounts.mu.Unlock()
	if got := switchOn(b, "the wrong passphrase entirely"); got != http.StatusForbidden {
		t.Fatalf("switched on with a wrong passphrase: %d", got)
	}
	if len(h.accounts.users) != 0 {
		t.Fatalf("a user made for a wrong passphrase: %v", h.accounts.users)
	}
	if got := switchOn(b, statePassphrase); got != http.StatusOK {
		t.Fatalf("switching on: %d", got)
	}
	if got := managementView(t, b); got == nil || !got.On || got.By != "admin@pve" || got.Since == nil || !got.Since.Equal(h.clock) || got.User != upgraderUser {
		t.Fatalf("after switching on: %+v", got)
	}
	if h.accounts.users[upgraderUser] == "" {
		t.Fatal("no Proxmox VE user made")
	}
	kept, err := os.ReadFile(filepath.Join(a.Dir, "unattended.json"))
	if err != nil || strings.Contains(string(kept), statePassphrase) || strings.Contains(string(kept), "5ec2e7") {
		t.Fatalf("kept in the open: %s, %v", kept, err)
	}
	if _, err := os.Stat(filepath.Join(a.KeyDir, "unattended.key")); err != nil {
		t.Fatalf("no key kept: %v", err)
	}
	if rec := b.do("GET", "/api/appliance/upgrades", ""); strings.Contains(rec.Body.String(), "5ec2e7") || strings.Contains(rec.Body.String(), "horse") {
		t.Fatalf("the view shows a secret: %s", rec.Body)
	}

	expect(t, d.do("DELETE", "/api/appliance/upgrades/management", ""), http.StatusForbidden)
	expect(t, b.do("DELETE", "/api/appliance/upgrades/management", ""), http.StatusOK)
	if got := managementView(t, b); got.On {
		t.Fatalf("after switching off: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "unattended.json")); !os.IsNotExist(err) {
		t.Fatalf("kept after switching off: %v", err)
	}
	if len(h.accounts.users) != 0 {
		t.Fatalf("the Proxmox VE user stayed: %v", h.accounts.users)
	}
}

// upgradeHour sets the appliance's upgrade hour, in UTC, with its own automatic upgrades off.
func upgradeHour(t *testing.T, up *Upgrades, hour int) {
	t.Helper()
	if err := os.WriteFile(up.SettingsPath, fmt.Appendf(nil, `{"policy":"off","hour":%d,"timeZone":"UTC"}`, hour), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticManagementUpgrade(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	up := withUpgrades(t, h)
	a := withManagementCluster(t, h)
	manual := a.Current().Started
	upgradeHour(t, up, 4)
	h.clock = time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)

	// Behind, in its hour, but not switched on.
	upgraded(t, a, "0.2.0")
	h.server.applyDueManagement()
	if j := a.Current(); j.Automatic || !j.Started.Equal(manual) {
		t.Fatalf("an automatic run without the opt-in: %+v", j)
	}

	if err := a.KeepUnattended(appliance.Unattended{Passphrase: statePassphrase, Token: upgraderUser + "!apply=5ec2e7", By: "admin@pve"}); err != nil {
		t.Fatal(err)
	}
	if !h.server.unattendedOn() {
		t.Fatal("not on once kept")
	}
	// An upgrade of the appliance asked for and not yet begun goes first.
	pending := filepath.Join(string(up.Spool), "requests", "upgrade.json")
	if err := os.WriteFile(pending, []byte(`{"policy":"patch"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.applyDueManagement()
	if a.Current().Automatic {
		t.Fatal("ran before the appliance's own upgrade")
	}
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	h.clock = time.Date(2026, 10, 14, 3, 30, 0, 0, time.UTC)
	h.server.applyDueManagement()
	if a.Current().Automatic {
		t.Fatal("ran outside its hour")
	}

	h.clock = time.Date(2026, 10, 14, 4, 1, 0, 0, time.UTC)
	h.server.applyDueManagement()
	j := jobEnded(t, a)
	if !j.Automatic || j.State != "succeeded" || j.User != upgraderUser {
		t.Fatalf("the automatic run: %+v", j)
	}
	b := h.browser()
	b.signIn("admin@pve")
	if got := setupUpgrade(t, b); got == nil || got.Behind || got.Applied != "0.2.0" {
		t.Fatalf("after the automatic run: %+v", got)
	}

	// Once up to date, nothing more runs, in this hour or the next day's.
	h.clock = time.Date(2026, 10, 15, 4, 1, 0, 0, time.UTC)
	h.server.applyDueManagement()
	if got := a.Current(); !got.Started.Equal(j.Started) {
		t.Fatalf("ran again with nothing to upgrade: %+v", got)
	}
}

// A failed automatic run leaves its record, and is tried again in the next day's hour, not before.
func TestFailedAutomaticManagementUpgradeWaitsADay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	up := withUpgrades(t, h)
	a := withManagementCluster(t, h)
	upgradeHour(t, up, 4)
	upgraded(t, a, "0.2.0")
	// The state no longer opens with what was kept, as after the passphrase was changed.
	if err := a.KeepUnattended(appliance.Unattended{Passphrase: "a passphrase of the past", Token: upgraderUser + "!apply=5ec2e7"}); err != nil {
		t.Fatal(err)
	}
	h.clock = time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	h.server.applyDueManagement()
	failed := jobEnded(t, a)
	if !failed.Automatic || failed.State != "failed" {
		t.Fatalf("the automatic run: %+v", failed)
	}
	b := h.browser()
	b.signIn("admin@pve")
	rec := b.do("GET", "/api/setup", "")
	if !strings.Contains(rec.Body.String(), `"automatic":true`) || !strings.Contains(rec.Body.String(), `"state":"failed"`) || !strings.Contains(rec.Body.String(), `"behind":true`) {
		t.Fatalf("setup = %s", rec.Body)
	}

	for _, at := range []time.Time{
		time.Date(2026, 10, 14, 4, 10, 0, 0, time.UTC),
		time.Date(2026, 10, 14, 4, 59, 0, 0, time.UTC),
	} {
		h.clock = at
		h.server.applyDueManagement()
		if got := a.Current(); !got.Started.Equal(failed.Started) {
			t.Fatalf("tried again at %s: %+v", at, got)
		}
	}
	// A restart remembers the attempt.
	restarted := &Upgrades{SettingsPath: up.SettingsPath, Now: up.Now}
	if restarted.managementDue(false) {
		t.Fatal("due again after a restart")
	}

	h.clock = time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC)
	h.server.applyDueManagement()
	if got := jobEnded(t, a); got.Started.Equal(failed.Started) || !got.Automatic {
		t.Fatalf("not tried the next day: %+v", got)
	}
}

// Removing the management cluster takes automatic upgrades with it, the Proxmox VE user included.
func TestRemovalSwitchesAutomaticUpgradesOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The removal runs on a renewed ticket, which lasts two hours from the harness's clock.
	h.clock = time.Now()
	withUpgrades(t, h)
	a := withManagementCluster(t, h)
	if err := a.KeepUnattended(appliance.Unattended{Passphrase: statePassphrase, Token: upgraderUser + "!apply=5ec2e7"}); err != nil {
		t.Fatal(err)
	}
	h.accounts.users = map[string]string{upgraderUser: upgraderUser + "!apply=5ec2e7"}
	b := h.browser()
	b.signIn("admin@pve")
	expect(t, b.do("POST", "/api/setup/remove", `{"passphrase":"`+statePassphrase+`","confirm":"remove"}`), http.StatusAccepted)
	if j := jobEnded(t, a); j.State != "succeeded" || !strings.Contains(strings.Join(j.Log, "\n"), "Switching automatic upgrades off") {
		t.Fatalf("removal = %+v", j)
	}
	if a.UnattendedOn() != nil {
		t.Fatal("still on after the removal")
	}
	if len(h.accounts.users) != 0 {
		t.Fatalf("the Proxmox VE user stayed: %v", h.accounts.users)
	}
	if a.LastApplied() != nil {
		t.Fatal("a removed management cluster is still recorded as applied")
	}
}
