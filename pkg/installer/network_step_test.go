package installer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	linkCheck   = "pct exec 100 -- sh -c ip -o link show dev eth0"
	nameLookup  = "pct exec 100 -- getent hosts pve1.example.com"
	apiConnect  = "pct exec 100 -- timeout 5 bash -c"
	gwPing      = "pct exec 100 -- ping"
	gwNeighbour = "pct exec 100 -- ip neigh show 192.0.2.1 dev eth0"
)

// networkRunner is a runner at the network step of container 100 at 192.0.2.77, whose API is
// pve1.example.com. It counts its pauses.
func networkRunner(t *testing.T, h *harness) (*Runner, *int) {
	t.Helper()
	r := h.runner(h.session(Config{CTID: "100", ProxmoxURL: "https://pve1.example.com:8006"}), nil)
	r.address = "192.0.2.77"
	pauses := new(int)
	r.Sleep = func(time.Duration) { *pauses++ }
	return r, pauses
}

func networkError(t *testing.T, err error) *UserError {
	t.Helper()
	fault, ok := errors.AsType[*UserError](err)
	if !ok {
		t.Fatalf("want a UserError, got %v", err)
	}
	return fault
}

// An API that takes a few tries to connect, as right after the container starts, passes the step.
func TestNetworkStepWaitsForTheAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(apiConnect, 1)
	r, pauses := networkRunner(t, h)
	r.Sleep = func(time.Duration) {
		*pauses++
		if *pauses == 3 {
			h.exec.on(apiConnect, "")
		}
	}
	if err := r.stepNetwork(context.Background()); err != nil {
		t.Fatalf("a connection on the fourth try failed the step: %v", err)
	}
	if n := h.exec.count(apiConnect); n != 4 {
		t.Errorf("want 4 connection tries, got %d", n)
	}
	if n := h.exec.count(gwPing); n != 0 {
		t.Errorf("a passing check pinged the gateway %d times", n)
	}
}

// An API that never connects behind a gateway that answers ARP is the API's fault, named after
// every try.
func TestNetworkStepGatewayAnswersButTheAPIDoesNot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(apiConnect, 1)
	h.exec.fail(gwPing, 1)
	h.exec.on(gwNeighbour, "192.0.2.1 lladdr 02:00:00:00:00:01 REACHABLE\n")
	r, pauses := networkRunner(t, h)
	fault := networkError(t, r.stepNetwork(context.Background()))
	if n := h.exec.count(apiConnect); n != netTries || *pauses != netTries-1 {
		t.Errorf("want %d tries and %d pauses, got %d and %d", netTries, netTries-1, n, *pauses)
	}
	if fault.Setting != settingAPI || !strings.Contains(fault.Cause, "reaches its gateway but not the Proxmox VE API at pve1.example.com:8006") {
		t.Errorf("want the gateway reachable and the API not, got %q (%s)", fault.Error(), fault.Setting)
	}
}

// A gateway that answers ping has answered ARP, also when the neighbour table cannot be read.
func TestNetworkStepGatewayAnswersPing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(apiConnect, 1)
	h.exec.fail(gwNeighbour, 1)
	r, _ := networkRunner(t, h)
	fault := networkError(t, r.stepNetwork(context.Background()))
	if fault.Setting != settingAPI || !strings.Contains(fault.Cause, "reaches its gateway but not the Proxmox VE API") {
		t.Errorf("want the gateway reachable and the API not, got %q (%s)", fault.Error(), fault.Setting)
	}
}

// A gateway that answers neither ARP nor ping points at the network and its VLAN.
func TestNetworkStepNothingAnswers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(apiConnect, 1)
	h.exec.fail(gwPing, 1)
	h.exec.on(gwNeighbour, "192.0.2.1 dev eth0 FAILED\n")
	r, _ := networkRunner(t, h)
	fault := networkError(t, r.stepNetwork(context.Background()))
	if fault.Setting != settingNetwork || !strings.Contains(fault.Cause, "at 192.0.2.77 cannot reach gateway 192.0.2.1 (no answer to ARP") ||
		!strings.Contains(fault.Fix, "--bridge and --vlan") {
		t.Errorf("want the network and VLAN named, got %q (%s)", fault.Error(), fault.Setting)
	}
}

// A DHCP container whose gateway does not answer gets the network to change, since its gateway
// comes from the DHCP server.
func TestNetworkStepDHCPGatewayDoesNotAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(apiConnect, 1)
	h.exec.fail(gwPing, 1)
	h.exec.on(gwNeighbour, "192.0.2.1 dev eth0 INCOMPLETE\n")
	r, _ := networkRunner(t, h)
	r.S.Cfg.IP = optDHCP
	if fault := networkError(t, r.stepNetwork(context.Background())); fault.Setting != settingNetwork {
		t.Errorf("want the network named, got %q (%s)", fault.Error(), fault.Setting)
	}
}

// An IPv6-only container names its IPv6 address and neighbour discovery, never ARP or an empty
// address.
func TestNetworkStepIPv6OnlyGatewayDoesNotAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(apiConnect, 1)
	h.exec.fail(gwPing, 1)
	h.exec.on("pct exec 100 -- ip -4 route show default", "")
	h.exec.on("pct exec 100 -- ip -6 route show default", "default via 2001:db8::1 dev eth0 metric 1024\n")
	h.exec.on("pct exec 100 -- ip neigh show 2001:db8::1 dev eth0", "2001:db8::1 dev eth0 FAILED\n")
	r, _ := networkRunner(t, h)
	r.S.Cfg.IP, r.S.Cfg.IP6, r.address, r.address6 = optNone, "2001:db8::77/64", "", "2001:db8::77"
	fault := networkError(t, r.stepNetwork(context.Background()))
	if fault.Setting != settingNetwork || !strings.Contains(fault.Cause, "at 2001:db8::77 cannot reach gateway 2001:db8::1 (no answer to neighbour discovery") ||
		strings.Contains(fault.Cause, "ARP") {
		t.Errorf("want the IPv6 address and neighbour discovery, got %q (%s)", fault.Error(), fault.Setting)
	}
}

// Without a default route the setting named follows how the container gets its address, and the
// fix names the flag to pass.
func TestNetworkStepNoRouteNamesTheSetting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip, ip6, setting, flag string
	}{
		{"192.0.2.77/24", "", settingV4Gateway, "--gateway"},
		{optDHCP, "", settingNetwork, "--bridge and --vlan"},
		{optNone, "2001:db8::77/64", settingV6Gateway, "--gateway6"},
		{optNone, optAuto, settingV6Mode, "--ip6 and --gateway6"},
	}
	for _, c := range cases {
		h := newHarness(t, opts{})
		h.exec.fail(apiConnect, 1)
		h.exec.on("pct exec 100 -- ip -4 route show default", "")
		r, _ := networkRunner(t, h)
		r.S.Cfg.IP, r.S.Cfg.IP6 = c.ip, c.ip6
		fault := networkError(t, r.stepNetwork(context.Background()))
		if fault.Setting != c.setting || !strings.Contains(fault.Fix, c.flag) || !strings.Contains(fault.Cause, "has no default route") {
			t.Errorf("--ip %s --ip6 %s: want %s and %s, got %q (%s)", c.ip, c.ip6, c.setting, c.flag, fault.Error(), fault.Setting)
		}
	}
}

// Plain mode prints a failure as it is, so each fix names the flags that change it.
func TestNetworkStepFixesNameTheFlags(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(nameLookup, 2)
	r, _ := networkRunner(t, h)
	if fault := networkError(t, r.stepNetwork(context.Background())); !strings.Contains(fault.Fix, "--dns ") || !strings.Contains(fault.Fix, "--proxmox-url ") {
		t.Errorf("the DNS fix names no flag: %q", fault.Fix)
	}
	h = newHarness(t, opts{})
	h.exec.fail(linkCheck, 1)
	r, _ = networkRunner(t, h)
	if fault := networkError(t, r.stepNetwork(context.Background())); !strings.Contains(fault.Fix, "--bridge, --vlan") {
		t.Errorf("the link fix names no flag: %q", fault.Fix)
	}
	h = newHarness(t, opts{})
	h.exec.fail(apiConnect, 1)
	r, _ = networkRunner(t, h)
	if fault := networkError(t, r.stepNetwork(context.Background())); !strings.Contains(fault.Fix, "--proxmox-url") {
		t.Errorf("the API fix names no flag: %q", fault.Fix)
	}
	if got := neutral("Check that --bridge and --vlan carry it, or give another URL with --proxmox-url, a CA with --proxmox-ca <file>"); strings.Contains(got, "--") {
		t.Errorf("the screen's wording keeps a flag: %q", got)
	}
}

// A name of the API that never resolves is the container's DNS.
func TestNetworkStepNameDoesNotResolve(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(nameLookup, 2)
	r, _ := networkRunner(t, h)
	fault := networkError(t, r.stepNetwork(context.Background()))
	if n := h.exec.count(nameLookup); n != netTries {
		t.Errorf("want %d lookups, got %d", netTries, n)
	}
	if fault.Setting != settingDNS || !strings.Contains(fault.Cause, "cannot resolve any name of the Proxmox VE API (tried pve1.example.com)") {
		t.Errorf("want DNS named, got %q (%s)", fault.Error(), fault.Setting)
	}
	if n := h.exec.count(apiConnect); n != 0 {
		t.Errorf("an unresolved name was connected to %d times", n)
	}
}

// An interface that never gets a carrier stops the step before any name is looked up.
func TestNetworkStepLinkNeverComesUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail(linkCheck, 1)
	r, pauses := networkRunner(t, h)
	fault := networkError(t, r.stepNetwork(context.Background()))
	if n := h.exec.count(linkCheck); n != netTries || *pauses != netTries-1 {
		t.Errorf("want %d link checks and %d pauses, got %d and %d", netTries, netTries-1, n, *pauses)
	}
	if fault.Setting != settingNetwork || !strings.Contains(fault.Cause, "container 100's network interface has no link") {
		t.Errorf("want the missing link named, got %q (%s)", fault.Error(), fault.Setting)
	}
	if n := h.exec.count(nameLookup); n != 0 {
		t.Errorf("a down link went on to %d lookups", n)
	}
}

// linkUp passes the link check and runs every other command for real.
type linkUp struct{ *OSExec }

func (l linkUp) Run(ctx context.Context, name string, args ...string) (string, error) {
	if strings.HasPrefix(strings.Join(append([]string{name}, args...), " "), linkCheck) {
		return "", nil
	}
	return l.OSExec.Run(ctx, name, args...)
}

// The install log keeps each try's command and its error, so a failed check can be traced. With
// no pct on the PATH every lookup fails.
func TestNetworkStepLogsEachTry(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	h := newHarness(t, opts{})
	var log bytes.Buffer
	r, _ := networkRunner(t, h)
	r.Exec = linkUp{&OSExec{Log: &log}}
	if fault := networkError(t, r.stepNetwork(context.Background())); fault.Setting != settingDNS {
		t.Errorf("want DNS named, got %q (%s)", fault.Error(), fault.Setting)
	}
	text := log.String()
	if n := strings.Count(text, "$ "+nameLookup+"\n"); n != netTries {
		t.Errorf("want %d lookups in the log, got %d:\n%s", netTries, n, text)
	}
	if n := strings.Count(text, "error: "); n != netTries {
		t.Errorf("want %d errors in the log, got %d:\n%s", netTries, n, text)
	}
}
