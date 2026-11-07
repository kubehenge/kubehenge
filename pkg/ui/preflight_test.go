package ui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const preflightSettings = `{"settings":{"nodes":["pve1"],"imageDatastore":"local","vmDatastore":"local-lvm","bridge":"vmbr0",
"nodeAddresses":["10.0.0.11/24"],"apiAddress":"10.0.0.10","gateway":"10.0.0.1","dnsServers":["10.0.0.1"],
"workloadDatastores":["local-lvm"],"workloadBridges":["vmbr0"],"bootstrapDatastore":"local"}}`

var preflightApply = strings.TrimSuffix(preflightSettings, "}") + `,"passphrase":"correct horse battery staple"}`

func preflightHarness(t *testing.T) (*harness, *browser) {
	t.Helper()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir(), ProxmoxURL: "https://pve.example.org:8006", Log: slog.New(slog.DiscardHandler)}
	h.server.Probe = func(context.Context, []netip.Addr) map[netip.Addr]bool { return nil }
	b := h.browser()
	b.signIn("admin@pve")
	return h, b
}

func checkFindings(t *testing.T, b *browser) appliance.Findings {
	t.Helper()
	return checkSettings(t, b, preflightSettings)
}

func checkSettings(t *testing.T, b *browser, settings string) appliance.Findings {
	t.Helper()
	rec := b.do("POST", "/api/setup/check", settings)
	expect(t, rec, http.StatusOK)
	var f appliance.Findings
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSetupCheckPassesAHealthyHost(t *testing.T) {
	t.Parallel()
	_, b := preflightHarness(t)
	f := checkFindings(t, b)
	if len(f.Errors) != 0 || len(f.PodRanges) != 1 {
		t.Errorf("findings = %+v", f)
	}
}

func TestSetupCheckNeedsTheSetupPrivileges(t *testing.T) {
	t.Parallel()
	h, _ := preflightHarness(t)
	b := h.browser()
	b.signIn("viewer@pve")
	expect(t, b.do("POST", "/api/setup/check", preflightSettings), http.StatusForbidden)
}

func TestSetupRefusesWhatProxmoxVEWouldFailAndStartsNothing(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	h.accounts.storages = []pve.Storage{
		{Storage: "local-lvm", Node: "pve1", Content: []string{"images"}, Active: true, Avail: 100 << 30},
		{Storage: "local", Node: "pve1", Content: []string{"iso", "vztmpl"}, Active: true, Avail: 100 << 30},
	}
	h.accounts.names = pve.Names{Pools: map[string]bool{"kubehenge": true}}
	h.server.Probe = func(context.Context, []netip.Addr) map[netip.Addr]bool {
		return map[netip.Addr]bool{netip.MustParseAddr("10.0.0.11"): true}
	}
	f := checkFindings(t, b)
	joined := strings.Join(f.Errors, "\n")
	for _, want := range []string{
		"Storage local on node pve1 lacks the import content type. Add it under Datacenter, Storage, Edit, Content.",
		"Pool kubehenge exists in Proxmox VE already",
		"Address 10.0.0.11 answers",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the check lacks %q in:\n%s", want, joined)
		}
	}
	rec := b.do("POST", "/api/setup", preflightApply)
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "lacks the import content type") {
		t.Errorf("the refusal = %s", rec.Body.String())
	}
	if h.server.Appliance.Current() != nil {
		t.Error("a run started although the pre-flight failed")
	}
}

func TestSetupWarningsDoNotStopTheRun(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	h.accounts.storages = []pve.Storage{
		{Storage: "local-lvm", Node: "pve1", Type: "lvmthin", Content: []string{"images"}, Active: true, Avail: 5 << 30},
		{Storage: "local", Node: "pve1", Type: "dir", Content: []string{"iso", "import"}, Active: true, Avail: 100 << 30},
	}
	f := checkFindings(t, b)
	if len(f.Errors) != 0 || !strings.Contains(strings.Join(f.Warnings, "\n"), "Writes fail once it fills") {
		t.Fatalf("findings = %+v, want the thin storage as a warning only", f)
	}
	// A passphrase that is too short is refused by the run itself, after the pre-flight passed, which
	// shows the warning did not stop it and starts nothing.
	rec := b.do("POST", "/api/setup", strings.Replace(preflightApply, "correct horse battery staple", "short", 1))
	expect(t, rec, http.StatusUnprocessableEntity)
	if strings.Contains(rec.Body.String(), "of disks.") || !strings.Contains(rec.Body.String(), "assphrase") {
		t.Errorf("the refusal = %s, want the passphrase's", rec.Body.String())
	}
}

func TestSetupCheckRefusesAFullNextIDBand(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	h.accounts.nextID = pve.NextID{Lower: 500, Upper: 502}
	h.accounts.vmids = []int{500, 501}
	f := checkFindings(t, b)
	mentions(t, f.Errors, "VM IDs 500 to 501", "0 IDs free", "Widen next-id under Datacenter, Options")
	rec := b.do("POST", "/api/setup", preflightApply)
	expect(t, rec, http.StatusUnprocessableEntity)
	if h.server.Appliance.Current() != nil {
		t.Error("a run started although the VM ID band is full")
	}
}

func TestSetupCheckFallsBackWhenNextIDIsForbidden(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	h.accounts.nextIDForbidden = true
	h.accounts.vmids = []int{10000}
	if f := checkFindings(t, b); len(f.Errors) != 0 {
		t.Errorf("findings = %+v, want the default range to pass", f)
	}
}

func mentions(t *testing.T, messages []string, parts ...string) {
	t.Helper()
	for _, m := range messages {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(m, p)
		}
		if ok {
			return
		}
	}
	t.Errorf("no message holds all of %q in %q", parts, messages)
}

func TestSetupCheckReportsSettingsThatAreWrong(t *testing.T) {
	t.Parallel()
	_, b := preflightHarness(t)
	rec := b.do("POST", "/api/setup/check", strings.Replace(preflightSettings, `"gateway":"10.0.0.1"`, `"gateway":"nope"`, 1))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "Gateway: an IPv4 address") {
		t.Errorf("answer = %s", rec.Body.String())
	}
}
