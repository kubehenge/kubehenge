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
	if fault.Setting != settingAPI || !strings.Contains(fault.Cause, "reaches its gateway 192.0.2.1 but cannot connect to the Proxmox VE API at pve1.example.com on port 8006") {
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
	if fault.Setting != settingAPI || !strings.Contains(fault.Cause, "reaches its gateway 192.0.2.1") || !strings.Contains(fault.Cause, "(its gateway answers ping)") {
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
	if fault.Setting != settingV4Gateway || !strings.Contains(fault.Cause, "no answer to ARP") || !strings.Contains(fault.Fix, "network and VLAN") {
		t.Errorf("want the network and VLAN named, got %q (%s)", fault.Error(), fault.Setting)
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
