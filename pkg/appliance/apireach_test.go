package appliance

import (
	"net/netip"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// firewalled is the healthy inventory with the datacenter firewall on and the node's own network
// 10.0.0.0/24 as its local network.
func firewalled() *Inventory {
	inv := healthy()
	inv.APIReach = &pve.APIReach{
		Enabled: true,
		Nodes: []pve.NodeReach{{
			Node: "pve1", Address: netip.MustParseAddr("10.0.0.5"),
			Interfaces: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		}},
	}
	return inv
}

func TestPreflightNamesManagementAddressesTheNodesFirewallWouldCutOff(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = []string{"10.1.0.11/24"}
	inv := firewalled()
	inv.ApplianceAddresses = []netip.Addr{netip.MustParseAddr("10.0.0.30"), netip.MustParseAddr("10.2.0.4")}
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

func TestPreflightChecksIPv6AddressesOfTheManagementNodes(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddressesIPv6 = []string{"2001:db8:9::11/64"}
	f := s.Preflight(firewalled(), false)
	mentions(t, f.Errors, "cut off 2001:db8:9::11")
}

func TestPreflightWarnsWhenTheLocalNetworkCannotBeTold(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	inv := firewalled()
	inv.APIReach.Nodes[0].Interfaces = nil
	inv.ApplianceAddresses = []netip.Addr{netip.MustParseAddr("10.2.0.4")}
	f := s.Preflight(inv, false)
	mentions(t, f.Warnings, "Could not tell", "the local network of node pve1", "IPSet management")
	for _, e := range f.Errors {
		t.Errorf("error: %s", e)
	}
}
