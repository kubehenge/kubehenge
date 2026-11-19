package installer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// --yes, --dry-run and --keep-failed reach the configuration.
func TestRunFlags(t *testing.T) {
	t.Parallel()
	f := pflag.NewFlagSet("install", pflag.ContinueOnError)
	var c Config
	BindFlags(f, &c)
	if err := f.Parse([]string{"--yes", "--dry-run", "--keep-failed"}); err != nil {
		t.Fatal(err)
	}
	if !c.Yes || !c.DryRun || !c.KeepFailed {
		t.Fatalf("want all three set, got yes %v, dry run %v, keep failed %v", c.Yes, c.DryRun, c.KeepFailed)
	}
}

func faultSetting(t *testing.T, err error) string {
	t.Helper()
	if fault, ok := errors.AsType[*UserError](err); ok {
		return fault.Setting
	}
	if na, ok := errors.AsType[*NoAddressError](err); ok {
		return na.Setting
	}
	t.Fatalf("want an error naming a setting, got %v", err)
	return ""
}

// A template storage with no path names the template setting, so the installer offers to change it.
func TestTemplateStorageWithoutPathNamesTheSetting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail("pvesm path", 2)
	r := h.runner(h.session(Config{TemplateStorage: "local-lvm"}), nil)
	if _, err := r.templatePath(context.Background()); faultSetting(t, err) != settingTemplate {
		t.Fatalf("want the template storage named, got %v", err)
	}
}

// An id another guest took during the run names the id setting, before the create and from its
// refusal alike.
func TestTakenIDNamesTheSetting(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		take func(*harness)
	}{
		{"before the create", func(h *harness) { h.exec.fail("pvesh get /cluster/nextid --vmid 100", 2) }},
		{"refused by the create", func(h *harness) { h.exec.failWith("pct create", "CT 100 already exists on node pve1") }},
	} {
		h := newHarness(t, opts{})
		c.take(h)
		r := h.runner(h.session(Config{CTID: "100", Storage: "local-lvm", TemplateStorage: "local-lvm", IP: "dhcp"}), nil)
		if err := r.stepCreate(context.Background()); faultSetting(t, err) != settingCTID {
			t.Errorf("%s: want the id named, got %v", c.name, err)
		}
	}
}

// A container without IPv6 has it switched off once it starts, and its IPv6 address is never read.
func TestIPv4OnlyContainerSwitchesIPv6Off(t *testing.T) {
	t.Parallel()
	off := "pct exec 100 -- sh -c " + ipv4OnlyScript
	for _, c := range []struct {
		ip6  string
		offs int
	}{{"", 1}, {"auto", 0}} {
		h := newHarness(t, opts{})
		r := h.runner(h.session(Config{CTID: "100", IP: "dhcp", IP6: c.ip6}), nil)
		if err := r.stepStart(context.Background()); err != nil {
			t.Fatalf("IPv6 %q: %v", c.ip6, err)
		}
		if n := h.exec.count(off); n != c.offs {
			t.Errorf("IPv6 %q: IPv6 switched off %d times, want %d", c.ip6, n, c.offs)
		}
		r.addresses(context.Background())
		if n := h.exec.count("pct exec 100 -- ip -6"); (n == 0) != (c.ip6 == "") {
			t.Errorf("IPv6 %q: the IPv6 address was read %d times", c.ip6, n)
		}
	}
	h := newHarness(t, opts{})
	h.exec.fail("pct exec 100 -- sh -c", 1)
	r := h.runner(h.session(Config{CTID: "100", IP: "dhcp"}), nil)
	if err := r.stepStart(context.Background()); err == nil || !strings.Contains(err.Error(), "would not switch IPv6 off") {
		t.Fatalf("want the failed switch named, got %v", err)
	}
}

// A container that gets no IPv6 address names the IPv6 setting.
func TestNoIPv6AddressNamesTheSetting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	r := h.runner(h.session(Config{CTID: "100", IP: optNone, IP6: "auto"}), nil)
	if err := r.stepAddress(context.Background()); faultSetting(t, err) != settingV6Mode {
		t.Fatalf("want the IPv6 setting named, got %v", err)
	}
}
