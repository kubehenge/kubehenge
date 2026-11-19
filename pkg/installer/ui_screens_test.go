package installer

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func (u *ui) up(n int) {
	u.t.Helper()
	for range n {
		u.press(tea.KeyPressMsg{Code: tea.KeyUp})
	}
}

// answer presses enter and waits for the next question.
func (u *ui) answer(next string) {
	u.t.Helper()
	u.enter(1)
	u.waitFor(next)
}

// manyNetworks gives the node the networks of a larger site: two bridges and n SDN VNets, the
// first of them tagged and carrying a subnet.
func manyNetworks(h *harness, n int) {
	vnets := make([]string, n)
	for i := range n {
		vnets[i] = fmt.Sprintf(`{"vnet":"vnet%02d","zone":"zone1","tag":"%d"}`, i+1, 20+i)
	}
	vnets[0] = `{"vnet":"vnet01","zone":"zone1","tag":"20","alias":"servers"}`
	h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr0"},{"type":"bridge","iface":"vmbr1","bridge_vlan_aware":1}]`)
	h.exec.on("pvesh get /cluster/sdn/vnets", "["+strings.Join(vnets, ",")+"]")
	h.exec.on("pvesh get /cluster/sdn/vnets/vnet01/subnets", `[{"cidr":"198.51.100.0/24","gateway":"198.51.100.1"}]`)
	h.exec.on("ip -4 -o addr show dev vmbr1", "")
}

// An address outside the subnet of the network is asked about before it is used, and the answer
// decides whether the installer goes on with it.
func TestUIAddressOutsideTheSubnetIsConfirmed(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{})
			u := startUIWith(t, h, Config{PVEEntry: "no", IP: "198.51.100.40/24", Gateway: "198.51.100.1"}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.answer("VLAN")
			u.answer("DNS name of the interface")
			u.answer("Certificate")
			u.answer("Use it anyway?")
			u.golden("outside")
			u.key('y')
			u.toSummary()
			if !u.s.AllowOutside || !strings.Contains(u.rec.screen(), "198.51.100.40/24") {
				t.Fatalf("the address was not kept:\n%s", u.rec.screen())
			}
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.enter(1)
			u.finish()
		})
	}
}

// The IPv6 questions: the address mode, the static address and gateway, and the dual-stack summary.
func TestUIIPv6Forms(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{})
			u := startUIWith(t, h, Config{PVEEntry: "no"}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.enter(1)
			u.waitFor("IP version")
			u.down(1)
			u.enter(1)
			u.waitFor("VLAN")
			u.answer("IPv6 address")
			u.golden("mode")
			u.down(2)
			u.enter(1)
			u.waitFor("IPv6 address and prefix")
			u.golden("static")
			u.typed("2001:db8::40/64")
			u.toSummary()
			u.golden("summary-ipv6-only")
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.enter(1)
			u.waitFor("/ filter")
			u.down(5)
			u.enter(1)
			u.waitFor("IPv6 gateway")
			u.golden("gateway")
			u.typed("2001:db8::1")
			u.toSummary()
			if !strings.Contains(u.rec.screen(), "2001:db8::40/64 via 2001:db8::1") {
				t.Fatalf("the summary lacks the IPv6 address:\n%s", u.rec.screen())
			}
			u.enter(1)
			u.waitFor("Kubehenge is running")
			u.enter(1)
			u.finish()
			if create := h.exec.last("pct create"); !strings.Contains(create, "ip6=2001:db8::40/64,gw6=2001:db8::1") || strings.Contains(create, ",ip=") {
				t.Fatalf("not created with the IPv6 address alone: %s", create)
			}
		})
	}
}

// Both families: the IPv4 address is static, the IPv6 address comes from router advertisements.
func TestUIDualStackSummary(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{})
			u := startUIWith(t, h, Config{PVEEntry: "no"}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.enter(1)
			u.waitFor("IP version")
			u.down(2)
			u.enter(1)
			u.toSummary()
			u.golden("summary-dual")
			for _, want := range []string{"192.0.2.254/24 via 192.0.2.1", "router advertisements"} {
				if !strings.Contains(u.rec.screen(), want) {
					t.Errorf("the summary lacks %q:\n%s", want, u.rec.screen())
				}
			}
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.enter(1)
			u.finish()
		})
	}
}

// The DNS question where the node's resolver is local, the two storage questions where several
// storages fit, and the question about the Proxmox VE entry.
func TestUIDNSStorageAndEntryForms(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{dhcp: true, rootStorages: 2, templateStorages: 2})
			writeTestFile(t, h.root+"/"+resolvConf, []byte("nameserver 127.0.0.53\n"))
			u := startUIWith(t, h, Config{}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.answer("IP version")
			u.answer("VLAN")
			u.answer("IPv4 address")
			u.answer("DNS servers")
			u.golden("dns")
			u.typed("192.0.2.53")
			u.answer("DNS name of the interface")
			u.answer("Certificate")
			u.answer("Disk storage")
			u.golden("storage")
			u.answer("Template storage")
			u.golden("template")
			u.answer("A Kubernetes button")
			u.golden("entry")
			u.answer("If it fails")
			if !strings.Contains(u.rec.screen(), "192.0.2.53") {
				t.Fatalf("the summary lacks the DNS server:\n%s", u.rec.screen())
			}
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.enter(1)
			u.finish()
		})
	}
}

// Another network than the one the node's route is on leaves the route's VLAN behind, and asks for
// the VLAN again.
func TestUIChangingTheNetworkDropsTheRoutesVLAN(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr0"},{"type":"bridge","iface":"vmbr1"},{"type":"vlan","iface":"vmbr1.20"}]`)
	h.exec.on("pvesh get /nodes/pve1/network/vmbr1", `{"iface":"vmbr1","type":"bridge","bridge_vlan_aware":1}`)
	h.exec.on("ip -4 route show default", "default via 198.51.100.1 dev vmbr1.20\n")
	h.exec.on("ip -4 -o addr show dev vmbr1.20", "7: vmbr1.20    inet 198.51.100.10/24 scope global vmbr1.20\n")
	u := startUIWith(t, h, Config{PVEEntry: "no"}, 100, 40, func(*Session) {})
	u.waitFor("Node")
	u.answer("IP version")
	u.answer("(this node)")
	if u.s.Cfg.Bridge != "vmbr1" || u.s.Cfg.VLAN != "20" {
		t.Fatalf("the route's network is %q VLAN %q", u.s.Cfg.Bridge, u.s.Cfg.VLAN)
	}
	u.up(1)
	u.enter(1)
	u.waitFor("VLAN")
	if u.s.Cfg.Bridge != "vmbr0" || u.s.Cfg.VLAN != "" {
		t.Fatalf("the VLAN stayed: %q VLAN %q", u.s.Cfg.Bridge, u.s.Cfg.VLAN)
	}
	u.enter(1)
	u.toSummary()
	if strings.Contains(u.rec.screen(), "VLAN 20") || !strings.Contains(u.rec.screen(), "vmbr0") {
		t.Fatalf("the summary keeps the route's VLAN:\n%s", u.rec.screen())
	}
	u.press(tea.KeyPressMsg{Code: tea.KeyRight})
	u.press(tea.KeyPressMsg{Code: tea.KeyRight})
	u.enter(1)
	u.finish()
}

// Each network is one line with its kind, zone, tag, subnet and alias, the node's own marked.
func TestUINetworkListNamesEachNetwork(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{})
			manyNetworks(h, 2)
			u := startUIWith(t, h, Config{PVEEntry: "no"}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.answer("IP version")
			u.answer("(this node)")
			u.golden("networks")
			screen := u.rec.screen()
			for _, want := range []string{"vmbr0, bridge, subnet 192.0.2.0/24 (this node)", "vnet01, VNet, zone zone1, tag 20, subnet 198.51.100.0/24, alias servers"} {
				if !strings.Contains(screen, want) {
					t.Errorf("the list lacks %q:\n%s", want, screen)
				}
			}
			u.press(tea.KeyPressMsg{Code: tea.KeyEscape})
			u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			u.finish()
		})
	}
}

// With more networks than the screen has lines the list is filtered by typing, and what matches
// fits the 80 by 24 screen.
func TestUINetworkListFiltersAtEightyByTwentyFour(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{})
			manyNetworks(h, 13)
			u := startUIWith(t, h, Config{PVEEntry: "no"}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.answer("IP version")
			u.answer("(this node)")
			u.golden("all")
			if sz.l < 40 {
				u.typed("vnet1")
				u.golden("filtered")
				u.enter(1)
				u.waitFor("IPv4 address")
				if u.s.Cfg.Bridge != "vnet10" {
					t.Fatalf("the network is %q, want vnet10", u.s.Cfg.Bridge)
				}
			}
			u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			u.finish()
		})
	}
}

// Several cautions stay whole on the smallest screen, each with its fix, and the choice is still
// shown. One address is as long as an IPv4 address gets.
func TestUISummaryWithSeveralCautions(t *testing.T) {
	t.Parallel()
	cautions := []string{
		apiFamilyCaution("192.0.2.10"),
		apiFamilyCaution("198.51.100.200"),
		apiFamilyCaution("192.0.2.12"),
	}
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{dhcp: true})
			u := startUIWith(t, h, Config{PVEEntry: "no", ProxmoxURL: "https://pve1.example.com:8006"}, sz.w, sz.l, func(s *Session) {
				s.Cautions = cautions
			})
			u.waitFor("Node")
			u.toSummary()
			u.golden("cautions")
			screen := u.rec.screen()
			if !strings.Contains(screen, "Install") {
				t.Fatalf("the choice is cut:\n%s", screen)
			}
			flat := strings.Join(strings.Fields(screen), " ")
			for _, c := range cautions {
				if !strings.Contains(flat, c) {
					t.Errorf("the caution %q is cut:\n%s", c, screen)
				}
			}
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.enter(1)
			u.finish()
		})
	}
}

// The words of the screens: rows named by what they hold, the failure keeping the install step lit,
// and the failure's action and log on rows of their own.
func TestUIWording(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	u := startUIWith(t, h, Config{PVEEntry: "no"}, 100, 40, func(*Session) {})
	u.waitFor("Node")
	u.toSummary()
	summary := u.rec.screen()
	for _, want := range []string{"  Network ", "  Container ID ", "  Host name "} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary lacks the row %q:\n%s", want, summary)
		}
	}
	for _, banned := range []string{"Bridge", "Choices", "  Next "} {
		if strings.Contains(summary, banned) {
			t.Errorf("the summary says %q:\n%s", banned, summary)
		}
	}
	u.enter(1)
	u.waitFor("Stopped")
	failure := u.rec.screen()
	if strings.Count(failure, testLogPath) != 1 {
		t.Errorf("the log path is not on one row alone:\n%s", failure)
	}
	if !strings.Contains(failure, "Read the log for Proxmox VE's message") {
		t.Errorf("the action does not name the log:\n%s", failure)
	}
	u.down(2)
	u.enter(1)
	m := u.finish()
	if m.crumb() != crumbInstall {
		t.Errorf("the failure lights crumb %d, not Install", m.crumb())
	}
}
