package installer

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The appliance is told the bridge and VLAN tag its container is on, from Proxmox VE's own
// configuration of it, so that the controller holds its addresses off the clusters on that network.
func TestInstallRecordsTheContainersNetwork(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pct config 100", "net0: name=eth0,bridge=vmbr1,tag=30,ip=192.0.2.40/24,gw=192.0.2.1\n")
	out, err := plainOn(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", DNS: "192.0.2.53", PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	env := string(h.exec.pushed["/etc/kubehenge/appliance.env"])
	for _, want := range []string{"KUBEHENGE_APPLIANCE_BRIDGE=vmbr1\n", "KUBEHENGE_APPLIANCE_VLAN=30\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("appliance.env lacks %q:\n%s", want, env)
		}
	}
}

// An upgrade records the network the container is on now, before the appliance restarts with it.
func TestUpgradeRecordsTheContainersNetwork(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	h.exec.on("pct config 100", "hostname: kubehenge\ntags: kubehenge\nnet0: name=eth0,bridge=web,ip=dhcp\n")
	if _, err := r.Upgrade(context.Background(), out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	recorded := slices.IndexFunc(h.exec.calls, func(c string) bool {
		return strings.HasPrefix(c, "pct exec 100 -- sh -c") && strings.Contains(c, "KUBEHENGE_APPLIANCE_BRIDGE=$1") && strings.HasSuffix(c, " sh web ")
	})
	restarted := slices.IndexFunc(h.exec.calls, func(c string) bool { return strings.HasPrefix(c, "pct exec 100 -- flock") })
	if recorded < 0 || recorded > restarted {
		t.Errorf("the network is recorded before the restart: %v", h.exec.calls)
	}
	for _, tc := range []struct{ conf, bridge, vlan string }{
		{"net0: name=eth0,bridge=vmbr0,hwaddr=BC:24:11:2E:5D:6A,ip=dhcp,tag=7,type=veth\n", "vmbr0", "7"},
		{"net0: name=eth0,bridge=vmbr0\nnet1: name=eth1,bridge=vmbr9,tag=4\n", "vmbr0", ""},
		{"hostname: kubehenge\n", "", ""},
	} {
		h, r, _ := upgradeHarness(t)
		h.exec.on("pct config 100", tc.conf)
		if bridge, vlan := r.containerNetwork(context.Background()); bridge != tc.bridge || vlan != tc.vlan {
			t.Errorf("%q: %q %q, want %q %q", tc.conf, bridge, vlan, tc.bridge, tc.vlan)
		}
	}
}
