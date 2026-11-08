package ui

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

func dualStackBridges() []pve.Bridge {
	return []pve.Bridge{{
		Bridge: "vmbr0", Node: "pve1", CIDR: "10.0.0.5/24", Gateway: "10.0.0.1",
		CIDR6: "2001:db8:0:1::5/64", Gateway6: "2001:db8:0:1::1",
	}}
}

func TestFreeAddressesByFamily(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		body string
		want freeView
	}{
		"IPv4 by default": {
			body: `{"nodes":3}`,
			want: freeView{Network: "10.0.0.0/24", Endpoint: "10.0.0.20", Addresses: "10.0.0.21-10.0.0.23"},
		},
		"IPv4 named": {
			body: `{"nodes":3,"families":["IPv4"]}`,
			want: freeView{Network: "10.0.0.0/24", Endpoint: "10.0.0.20", Addresses: "10.0.0.21-10.0.0.23"},
		},
		"IPv6 alone": {
			body: `{"nodes":3,"families":["IPv6"]}`,
			want: freeView{Network6: "2001:db8:0:1::/64", Endpoint6: "2001:db8:0:1::14", Addresses6: "2001:db8:0:1::15-2001:db8:0:1::17"},
		},
		"dual stack": {
			body: `{"nodes":2,"families":["IPv6","IPv4"]}`,
			want: freeView{
				Network: "10.0.0.0/24", Endpoint: "10.0.0.20", Addresses: "10.0.0.21-10.0.0.22",
				Network6: "2001:db8:0:1::/64", Endpoint6: "2001:db8:0:1::14", Addresses6: "2001:db8:0:1::15-2001:db8:0:1::16",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.accounts.bridgeList = dualStackBridges()
			h.server.Probe = (&answeringOn{hosts: map[netip.Addr]bool{}}).probe
			b := h.browser()
			b.signIn("dev@pve")
			rec := b.do("POST", "/api/proxmox/nodes/pve1/bridges/vmbr0/free", c.body)
			expect(t, rec, http.StatusOK)
			var got freeView
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Answering) != 0 {
				t.Fatalf("answering = %v, want none", got.Answering)
			}
			got.Answering = nil
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("free = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestFreeAddressesRefuseAnUnknownFamily(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.accounts.bridgeList = dualStackBridges()
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("POST", "/api/proxmox/nodes/pve1/bridges/vmbr0/free", `{"nodes":1,"families":["IPv5"]}`), http.StatusBadRequest)
}

func TestFreeAddressesProbeEachFamilyTyped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.accounts.bridgeList = dualStackBridges()
	hosts := &answeringOn{hosts: map[netip.Addr]bool{netip.MustParseAddr("2001:db8:0:1::31"): true, netip.MustParseAddr("10.0.0.31"): true}}
	h.server.Probe = hosts.probe
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/proxmox/nodes/pve1/bridges/vmbr0/free",
		`{"nodes":0,"families":["IPv4","IPv6"],"check":["10.0.0.31","2001:db8:0:1::30-2001:db8:0:1::32","2001:db8:ffff::1"]}`)
	expect(t, rec, http.StatusOK)
	var got freeView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.31", "2001:db8:0:1::31"}; !slices.Equal(got.Answering, want) {
		t.Fatalf("answering = %v, want %v", got.Answering, want)
	}
	hosts.mu.Lock()
	defer hosts.mu.Unlock()
	if slices.Contains(hosts.probed, netip.MustParseAddr("2001:db8:ffff::1")) {
		t.Error("an address off the network was probed")
	}
}

func TestFreeRunIPv6(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		network string
		taken   []string
		answer  []string
		n       int
		want    []string
	}{
		"from the twentieth": {network: "2001:db8::/64", n: 2, want: []string{"2001:db8::14", "2001:db8::15"}},
		"small network skips the anycast address": {
			network: "2001:db8::/122", n: 2, want: []string{"2001:db8::1", "2001:db8::2"},
		},
		"around a taken range": {
			network: "2001:db8::/122", taken: []string{"2001:db8::2-2001:db8::4"}, n: 2, want: []string{"2001:db8::5", "2001:db8::6"},
		},
		"around a host that answers": {
			network: "2001:db8::/122", answer: []string{"2001:db8::3"}, n: 3, want: []string{"2001:db8::4", "2001:db8::6"},
		},
		"none free":      {network: "2001:db8::/126", answer: []string{"2001:db8::2"}, n: 3},
		"point to point": {network: "2001:db8::/127", n: 2, want: []string{"2001:db8::", "2001:db8::1"}},
		"last address":   {network: "2001:db8::/126", n: 3, want: []string{"2001:db8::1", "2001:db8::3"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hosts := &answeringOn{hosts: map[netip.Addr]bool{}}
			for _, a := range c.answer {
				hosts.hosts[netip.MustParseAddr(a)] = true
			}
			var taken [][2]netip.Addr
			for _, r := range c.taken {
				from, to, _ := strings.Cut(r, "-")
				taken = append(taken, [2]netip.Addr{netip.MustParseAddr(from), netip.MustParseAddr(to)})
			}
			s := &Server{Probe: hosts.probe}
			run := s.freeRun(t.Context(), netip.MustParsePrefix(c.network), taken, c.n)
			var got []string
			if len(run) > 0 {
				got = []string{run[0].String(), run[len(run)-1].String()}
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("run from %v, want %v", got, c.want)
			}
		})
	}
}

func TestHostsOfANetwork(t *testing.T) {
	t.Parallel()
	for network, want := range map[string][2]string{
		"192.0.2.0/24":   {"192.0.2.1", "192.0.2.254"},
		"192.0.2.0/31":   {"192.0.2.0", "192.0.2.1"},
		"2001:db8::/64":  {"2001:db8::1", "2001:db8::ffff:ffff:ffff:ffff"},
		"2001:db8::/127": {"2001:db8::", "2001:db8::1"},
		"2001:db8::/128": {"2001:db8::", "2001:db8::"},
		"2001:db8::/48":  {"2001:db8::1", "2001:db8:0:ffff:ffff:ffff:ffff:ffff"},
	} {
		first, last := hosts(netip.MustParsePrefix(network))
		if first.String() != want[0] || last.String() != want[1] {
			t.Errorf("hosts(%s) = %s, %s, want %s, %s", network, first, last, want[0], want[1])
		}
	}
}

func TestCheckedIPv6StaysOnTheNetwork(t *testing.T) {
	t.Parallel()
	network := netip.MustParsePrefix("2001:db8::/64")
	got := checked([]string{"2001:db7:ffff::1-2001:db8::2", "10.0.0.5", "2001:db9::1", "nonsense"}, network)
	text := make([]string, 0, len(got))
	for _, a := range got {
		text = append(text, a.String())
	}
	if want := []string{"2001:db8::1", "2001:db8::2"}; !slices.Equal(text, want) {
		t.Fatalf("checked = %v, want %v", text, want)
	}
	if got := checked([]string{"2001:db8::/64"}, network); len(got) != checkMost {
		t.Fatalf("a whole network checks %d addresses, want %d", len(got), checkMost)
	}
}

func TestDialNetworkIsTheAddressesFamily(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]string{"192.0.2.1": "tcp4", "2001:db8::1": "tcp6", "::1": "tcp6", "127.0.0.1": "tcp4"} {
		if got := dialNetwork(netip.MustParseAddr(addr)); got != want {
			t.Errorf("dialNetwork(%s) = %s, want %s", addr, got, want)
		}
	}
}

func TestEchoUsesICMPv6ForIPv6(t *testing.T) {
	t.Parallel()
	if ipv6Echo.socket != "udp6" || ipv6Echo.protocol != 58 || ipv6Echo.request.Protocol() != 58 || ipv6Echo.reply.Protocol() != 58 {
		t.Fatalf("IPv6 ping = %+v, want a udp6 socket and ICMPv6", ipv6Echo)
	}
	if ipv4Echo.socket != "udp4" || ipv4Echo.protocol != 1 || ipv4Echo.request.Protocol() != 1 || ipv4Echo.reply.Protocol() != 1 {
		t.Fatalf("IPv4 ping = %+v, want a udp4 socket and ICMP", ipv4Echo)
	}
	// Nothing to ping sends nothing and finds no one.
	if got := echoReplies(t.Context(), nil, slog.New(slog.DiscardHandler)); len(got) != 0 {
		t.Fatalf("replies from no addresses = %v", got)
	}
}

func TestProbeReachesLoopbackOfEachFamily(t *testing.T) {
	t.Parallel()
	// A closed port on loopback refuses the connection, which is an answer. Loopback is never an
	// address any listener here binds beyond.
	for _, addr := range []string{"127.0.0.1", "::1"} {
		a := netip.MustParseAddr(addr)
		if l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(addr, "0")); err != nil {
			t.Skipf("no %s loopback: %v", addr, err)
		} else {
			_ = l.Close()
		}
		got := probeAddresses(t.Context(), []netip.Addr{a}, slog.New(slog.DiscardHandler))
		if !got[a] {
			t.Errorf("%s did not answer, its closed ports refuse connections", addr)
		}
	}
}

func TestSetupAddressesByFamily(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		settings appliance.Settings
		want     []string
	}{
		"IPv4 unchanged": {
			settings: appliance.Settings{APIAddress: "192.0.2.10", NodeAddresses: []string{"192.0.2.11/24", "192.0.2.12/24"}},
			want:     []string{"192.0.2.10", "192.0.2.11", "192.0.2.12"},
		},
		"IPv6 alone": {
			settings: appliance.Settings{
				IPFamilies: []string{"IPv6"}, APIAddress: "2001:db8::10", NodeAddressesIPv6: []string{"2001:db8::11/64"},
			},
			want: []string{"2001:db8::10", "2001:db8::11"},
		},
		"dual stack": {
			settings: appliance.Settings{
				IPFamilies: []string{"IPv4", "IPv6"}, APIAddress: "192.0.2.10",
				NodeAddresses: []string{"192.0.2.11/24"}, NodeAddressesIPv6: []string{"2001:db8::11/64"},
			},
			want: []string{"192.0.2.10", "192.0.2.11", "2001:db8::11"},
		},
		"unreadable and mapped entries left out": {
			settings: appliance.Settings{APIAddress: "nonsense", NodeAddresses: []string{"::ffff:192.0.2.1/24", "192.0.2.5/24"}},
			want:     []string{"192.0.2.5"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, a := range setupAddresses(&c.settings) {
				got = append(got, a.String())
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("setupAddresses = %v, want %v", got, c.want)
			}
		})
	}
}

func TestBuildNetworkTakesEitherFamily(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		address, gateway string
		dns              []string
		problem          string
	}{
		"IPv4":                 {address: "192.0.2.50/24", gateway: "192.0.2.1", dns: []string{"192.0.2.2"}},
		"IPv6":                 {address: "2001:db8::50/64", gateway: "2001:db8::1", dns: []string{"2001:db8::2"}},
		"IPv6 with IPv4 DNS":   {address: "2001:db8::50/64", gateway: "2001:db8::1", dns: []string{"192.0.2.2"}},
		"gateway of the other": {address: "2001:db8::50/64", gateway: "192.0.2.1", dns: []string{"192.0.2.2"}, problem: "Gateway: an address in the same network"},
		"gateway off network":  {address: "2001:db8::50/64", gateway: "2001:db9::1", dns: []string{"2001:db8::2"}, problem: "Gateway: an address in the same network"},
		"mapped address":       {address: "::ffff:192.0.2.50/120", gateway: "::ffff:192.0.2.1", dns: []string{"192.0.2.2"}, problem: "Address: an IPv4 or IPv6 address"},
		"no prefix":            {address: "2001:db8::50", gateway: "2001:db8::1", dns: []string{"2001:db8::2"}, problem: "Address: an IPv4 or IPv6 address"},
		"gateway without addr": {gateway: "2001:db8::1", problem: "Gateway and DNS servers need an address"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, problem := buildNetwork(c.address, c.gateway, c.dns)
			if !strings.HasPrefix(problem, c.problem) || (c.problem == "") != (problem == "") {
				t.Fatalf("problem = %q, want %q", problem, c.problem)
			}
			if c.problem == "" && (n == nil || n.Address.String() != c.address) {
				t.Fatalf("network = %+v", n)
			}
		})
	}
}

func TestOwnAddressByFamily(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		cidr string
		is4  bool
		ok   bool
	}{
		{"10.0.0.5/24", true, true},
		{"10.0.0.5/24", false, false},
		{"2001:db8::5/64", false, true},
		{"2001:db8::5/64", true, false},
		{"::ffff:10.0.0.5/120", false, false},
		{"", true, false},
		{"nonsense", false, false},
	} {
		if _, ok := ownAddress(c.cidr, c.is4); ok != c.ok {
			t.Errorf("ownAddress(%q, %v) ok = %v, want %v", c.cidr, c.is4, ok, c.ok)
		}
	}
}
