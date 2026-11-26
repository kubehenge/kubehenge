package installer

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The appliance is given every network card Proxmox VE configures for its container, so that the
// controller holds each of its addresses off the clusters on that card's bridge and VLAN.
func TestInstallRecordsTheContainersNetwork(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pct config 100", "hostname: kubehenge\nnet1: name=eth1,bridge=vmbr0,ip=dhcp\nnet0: name=eth0,bridge=vmbr1,tag=30,ip=192.0.2.40/24,gw=192.0.2.1\n")
	out, err := plainOn(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", DNS: "192.0.2.53", PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "net0: name=eth0,bridge=vmbr1,tag=30,ip=192.0.2.40/24,gw=192.0.2.1\nnet1: name=eth1,bridge=vmbr0,ip=dhcp\n"
	if got := string(h.exec.pushed["/var/lib/kubehenge/network"]); got != want {
		t.Errorf("the appliance's network = %q, want %q", got, want)
	}
}

// Every upgrade records the network the container is on now, a retry after one cut off in the
// middle too, outside what an upgrade sets aside and a rollback puts back.
func TestUpgradeRecordsTheContainersNetwork(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	h.exec.on("pct config 100", "hostname: kubehenge\ntags: kubehenge\nnet0: name=eth0,bridge=web,ip=dhcp\n")
	for try := range 2 {
		h.exec.pushed = nil
		if _, err := r.Upgrade(context.Background(), out); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got := string(h.exec.pushed["/var/lib/kubehenge/network"]); got != "net0: name=eth0,bridge=web,ip=dhcp\n" {
			t.Errorf("upgrade %d: the appliance's network = %q", try+1, got)
		}
	}
	recorded := slices.IndexFunc(h.exec.calls, func(c string) bool {
		return strings.HasPrefix(c, "pct push 100 ") && strings.Contains(c, " /var/lib/kubehenge/network ")
	})
	upgraded := slices.IndexFunc(h.exec.calls, func(c string) bool { return strings.HasPrefix(c, "pct exec 100 -- flock") })
	if recorded < 0 || recorded > upgraded {
		t.Errorf("the network is recorded before the upgrade: %v", h.exec.calls)
	}
	if strings.Contains(upgradeScript, "/var/lib/kubehenge/") {
		t.Error("the upgrade script sets aside or puts back what is in the interface's state")
	}
}

// A configuration without cards, as one that could not be read, leaves what the appliance holds.
// The snapshots' sections are not the container's.
func TestUpgradeKeepsTheNetworkItCannotRead(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	h.exec.on("pct config 100", "hostname: kubehenge\ntags: kubehenge\n")
	if _, err := r.Upgrade(context.Background(), out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, ok := h.exec.pushed["/var/lib/kubehenge/network"]; ok {
		t.Errorf("a configuration without cards wrote %q", got)
	}
	entries := configEntries("net0: name=eth0,bridge=vmbr1\n\n[before]\nnet0: name=eth0,bridge=vmbr9\n")
	if entries["net0"] != "name=eth0,bridge=vmbr1" {
		t.Errorf("the container's card is %q", entries["net0"])
	}
}
