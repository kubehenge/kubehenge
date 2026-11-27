package installer

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/acme"
)

const (
	onThisNode  = `[{"type":"lxc","vmid":100,"node":"pve1","tags":"kubehenge"}]`
	onOtherNode = `[{"type":"lxc","vmid":100,"node":"elsewhere","tags":"kubehenge"}]`
	onTwoNodes  = `[{"type":"lxc","vmid":100,"node":"pve1","tags":"kubehenge"},{"type":"lxc","vmid":101,"node":"pve2","tags":"a;kubehenge"}]`
)

// findingHarness is a node with the appliance of upgradeHarness, listing resources as given.
func findingHarness(t *testing.T, resources string) *harness {
	t.Helper()
	h, _, _ := upgradeHarness(t)
	h.exec.on("pvesh get /cluster/resources", resources)
	h.exec.on("pct config 100", "hostname: kubehenge\ntags: kubehenge\nnet0: name=eth0,bridge=vmbr0,ip=192.0.2.40/24\n")
	return h
}

// runOn runs the options on the harness's node as Execute does, without a terminal.
func runOn(h *harness, cfg Config) (string, error) { //nolint:gocritic // tests pass a Config literal.
	c := h.session(cfg).Cfg
	var out strings.Builder
	err := executeOn(context.Background(), h.env(), &c, ioStreams{in: strings.NewReader(""), out: &out, errs: io.Discard}, false, "dumb")
	return out.String(), err
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %q", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Fatalf("got %q, want %q", err, p)
		}
	}
}

func TestUpgradeWithoutCTIDUpgradesTheAppliance(t *testing.T) {
	t.Parallel()
	h := findingHarness(t, onThisNode)
	out, err := runOn(h, Config{Upgrade: true})
	if err != nil || !strings.Contains(out, "Kubehenge 9.9.9 is running in container 100") || h.exec.count("pct exec 100 -- flock -n -E 75") != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestUpgradeWithoutCTIDOnAnotherNode(t *testing.T) {
	t.Parallel()
	_, err := runOn(findingHarness(t, onOtherNode), Config{Upgrade: true})
	wantErr(t, err, "container 100 runs on node elsewhere", "Run this there")
}

func TestUpgradeWithoutCTIDWithTwoAppliances(t *testing.T) {
	t.Parallel()
	_, err := runOn(findingHarness(t, onTwoNodes), Config{Upgrade: true})
	wantErr(t, err, "100 on pve1, 101 on pve2", "Name one with --ctid")
}

func TestUpgradeWithoutCTIDWithNoAppliance(t *testing.T) {
	t.Parallel()
	_, err := runOn(findingHarness(t, "[]"), Config{Upgrade: true})
	wantErr(t, err, "this cluster has no Kubehenge appliance", "without --upgrade")
}

func TestCertificateWithoutCTID(t *testing.T) {
	t.Parallel()
	h := findingHarness(t, onThisNode)
	out, err := runOn(h, Config{Certificate: true, Name: "kubehenge.example.org"})
	if err != nil || !strings.Contains(out, "https://kubehenge.example.org/") || h.exec.count("pct push 100") != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err = runOn(findingHarness(t, onOtherNode), Config{Certificate: true})
	wantErr(t, err, "container 100 runs on node elsewhere")
	_, err = runOn(findingHarness(t, onTwoNodes), Config{Certificate: true})
	wantErr(t, err, "100 on pve1, 101 on pve2", "Name one with --ctid")
	_, err = runOn(findingHarness(t, "[]"), Config{Certificate: true})
	wantErr(t, err, "this cluster has no Kubehenge appliance")
}

func TestPlainRunUpgradesTheAppliance(t *testing.T) {
	t.Parallel()
	h := findingHarness(t, onThisNode)
	out, err := runOn(h, Config{})
	if err != nil || !strings.HasPrefix(out, "Upgrading the appliance in container 100\n") || !strings.Contains(out, "Kubehenge 9.9.9 is running in container 100") {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.exec.count("pct create") != 0 || h.exec.count("pct exec 100 -- flock -n -E 75") != 1 {
		t.Fatalf("a plain run installed: %v", h.exec.calls)
	}
}

func TestPlainRunRefusesAnInstallOnlyOption(t *testing.T) {
	t.Parallel()
	h := findingHarness(t, onThisNode)
	out, err := runOn(h, Config{ACME: acme.Files{Directory: "letsencrypt"}})
	wantErr(t, err, "--acme works only with an install or --certificate")
	if out != "" || h.exec.changed() != 0 {
		t.Fatalf("the node was changed: %q %v", out, h.exec.calls)
	}
}

func TestPlainRunWithTheApplianceElsewhere(t *testing.T) {
	t.Parallel()
	_, err := runOn(findingHarness(t, onOtherNode), Config{})
	wantErr(t, err, "container 100 runs on node elsewhere", "Run this there")
	_, err = runOn(findingHarness(t, onTwoNodes), Config{})
	wantErr(t, err, "100 on pve1, 101 on pve2", "Name one with --ctid")
}

func TestPlainRunWithNoApplianceInstalls(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	out, err := runOn(h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err != nil || strings.Contains(out, "Upgrading") || !strings.Contains(out, "ok   Create container") || h.exec.count("pct create") != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestUpgradeWithCTIDStillWorks(t *testing.T) {
	t.Parallel()
	h := findingHarness(t, onTwoNodes)
	out, err := runOn(h, Config{Upgrade: true, CTID: "100"})
	if err != nil || !strings.Contains(out, "Kubehenge 9.9.9 is running in container 100") {
		t.Fatalf("%v\n%s", err, out)
	}
	h = findingHarness(t, "[]")
	h.exec.on("pct config 100", "hostname: other\n")
	_, err = runOn(h, Config{Upgrade: true, CTID: "100"})
	wantErr(t, err, "container 100 is not Kubehenge's appliance")
}
