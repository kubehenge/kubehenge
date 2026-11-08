package appliance

import (
	"net"
	"net/netip"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func addrs(texts ...string) []netip.Addr {
	out := make([]netip.Addr, len(texts))
	for i, t := range texts {
		out[i] = netip.MustParseAddr(t)
	}
	return out
}

// firewalled is the healthy inventory with the datacenter firewall on and the node's own network
// 10.0.0.0/24 as its local network, reached over IPv4 from inside it.
func firewalled() *Inventory {
	inv := healthy()
	inv.APIReach = &pve.APIReach{
		Enabled: true,
		Nodes: []pve.NodeReach{{
			Node: "pve1", Address: netip.MustParseAddr("10.0.0.5"),
			Interfaces: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		}},
	}
	inv.Route = APIRoute{Endpoints: addrs("10.0.0.5"), Sources: addrs("10.0.0.30")}
	return inv
}

func TestPreflightNamesManagementAddressesTheNodesFirewallWouldCutOff(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = []string{"10.1.0.11/24"}
	inv := firewalled()
	inv.Route.Sources = addrs("10.2.0.4")
	f := s.Preflight(inv, false)
	mentions(t, f.Errors, "cut off 10.1.0.11, 10.2.0.4", "Add 10.1.0.11, 10.2.0.4 to the datacenter firewall's IPSet management.")
}

func TestPreflightAcceptsAddressesTheIPSetManagementHolds(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = []string{"10.1.0.11/24"}
	inv := firewalled()
	inv.APIReach.Management = []pve.IPSetEntry{{CIDR: "10.1.0.0/24"}}
	if f := s.Preflight(inv, false); len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
}

func TestPreflightLeavesAFirewallThatIsOffAlone(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = []string{"10.1.0.11/24"}
	inv := firewalled()
	inv.APIReach.Enabled = false
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("findings = %+v", f)
	}
	inv.APIReach = nil
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("findings = %+v", f)
	}
}

func TestPreflightIgnoresAGlobalIPv6AddressThatDoesNotReachAnIPv4Endpoint(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddressesIPv6 = []string{"2001:db8:9::11/64"}
	inv := firewalled()
	// The appliance has a global IPv6 address too, but the kernel sends to the endpoint from IPv4.
	if f := s.Preflight(inv, false); len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
}

func TestPreflightChecksIPv6AddressesTowardsAnIPv6Endpoint(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddressesIPv6 = []string{"2001:db8:9::11/64"}
	inv := firewalled()
	inv.APIReach.Nodes[0] = pve.NodeReach{
		Node: "pve1", Address: netip.MustParseAddr("2001:db8:1::5"),
		Interfaces: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")},
	}
	inv.Route = APIRoute{Endpoints: addrs("2001:db8:1::5"), Sources: addrs("2001:db8:2::4")}
	f := s.Preflight(inv, false)
	mentions(t, f.Errors, "cut off 2001:db8:2::4, 2001:db8:9::11. Add")
}

func TestPreflightChecksBothFamiliesOfAHostNameThatResolvesToBoth(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = []string{"10.1.0.11/24"}
	s.NodeAddressesIPv6 = []string{"2001:db8:9::11/64"}
	inv := firewalled()
	inv.Route = APIRoute{Endpoints: addrs("10.0.0.5", "2001:db8:1::5"), Sources: addrs("10.0.0.30", "2001:db8:1::30")}
	f := s.Preflight(inv, false)
	// Both management nodes and the appliance's IPv6 source are outside the IPv4 local network and the
	// IPSet. The appliance's IPv4 source is inside.
	mentions(t, f.Errors, "cut off 10.1.0.11, 2001:db8:1::30, 2001:db8:9::11")
}

func TestPreflightWarnsWhenTheLocalNetworkCannotBeTold(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	inv := firewalled()
	inv.APIReach.Nodes[0].Interfaces = nil
	inv.Route.Sources = addrs("10.2.0.4")
	f := s.Preflight(inv, false)
	mentions(t, f.Warnings, "Could not tell", "the local network of node pve1", "IPSet management")
	for _, e := range f.Errors {
		t.Errorf("error: %s", e)
	}
}

func TestRouteToAPIFindsTheSourceTheKernelPicks(t *testing.T) {
	t.Parallel()
	got := RouteToAPI(t.Context(), "https://127.0.0.1:8006")
	if len(got.Endpoints) != 1 || got.Endpoints[0] != netip.MustParseAddr("127.0.0.1") || len(got.Sources) != 1 || got.Sources[0] != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("route = %+v", got)
	}
	if got := RouteToAPI(t.Context(), "https://localhost"); len(got.Endpoints) == 0 {
		t.Errorf("a host name gave no endpoint: %+v", got)
	}
	if got := RouteToAPI(t.Context(), "not a url"); len(got.Endpoints) != 0 {
		t.Errorf("route = %+v", got)
	}
	if ln, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		_ = ln.Close()
		v6 := RouteToAPI(t.Context(), "https://[::1]:8006")
		if len(v6.Sources) != 1 || v6.Sources[0] != netip.MustParseAddr("::1") {
			t.Errorf("IPv6 route = %+v", v6)
		}
	}
}
