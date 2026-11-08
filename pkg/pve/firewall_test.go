package pve

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func addrs(texts ...string) []netip.Addr {
	out := make([]netip.Addr, len(texts))
	for i, t := range texts {
		out[i] = netip.MustParseAddr(t)
	}
	return out
}

func node(name, own string, ifaces ...string) NodeReach {
	n := NodeReach{Node: name, Address: netip.MustParseAddr(own)}
	for _, i := range ifaces {
		n.Interfaces = append(n.Interfaces, netip.MustParsePrefix(i).Masked())
	}
	return n
}

func TestAPIReachCutNamesAddressesOutsideTheLocalNetworkAndManagement(t *testing.T) {
	t.Parallel()
	r := &APIReach{Enabled: true, Nodes: []NodeReach{node("pve1", "10.0.0.5", "10.0.0.5/24")}}
	got := r.Cut(addrs("10.0.0.20", "10.1.0.9", "192.168.5.5"))
	if len(got.Cut) != 2 || got.Cut[0].String() != "10.1.0.9" || got.Cut[1].String() != "192.168.5.5" {
		t.Errorf("cut = %v", got.Cut)
	}
	if len(got.Unsure) != 0 {
		t.Errorf("unsure = %v", got.Unsure)
	}
}

func TestAPIReachCutAcceptsTheManagementIPSet(t *testing.T) {
	t.Parallel()
	r := &APIReach{
		Enabled: true, Nodes: []NodeReach{node("pve1", "10.0.0.5", "10.0.0.5/24")},
		Management: []IPSetEntry{{CIDR: "10.1.0.0/24"}, {CIDR: "10.1.0.9", NoMatch: true}, {CIDR: "172.16.0.4"}},
		Aliases:    map[string]string{},
	}
	got := r.Cut(addrs("10.1.0.8", "10.1.0.9", "172.16.0.4"))
	if len(got.Cut) != 1 || got.Cut[0].String() != "10.1.0.9" {
		t.Errorf("cut = %v, want only the excluded 10.1.0.9", got.Cut)
	}
}

func TestAPIReachCutReadsAnAliasInTheIPSetAndSaysWhenItIsMissing(t *testing.T) {
	t.Parallel()
	r := &APIReach{
		Enabled: true, Nodes: []NodeReach{node("pve1", "10.0.0.5", "10.0.0.5/24")},
		Management: []IPSetEntry{{CIDR: "dc/appliance"}, {CIDR: "dc/gone"}},
		Aliases:    map[string]string{"appliance": "10.1.0.9"},
	}
	got := r.Cut(addrs("10.1.0.9", "10.1.0.10"))
	if len(got.Cut) != 1 || got.Cut[0].String() != "10.1.0.10" {
		t.Errorf("cut = %v", got.Cut)
	}
	if len(got.Unsure) != 1 || !strings.Contains(got.Unsure[0], "dc/gone") {
		t.Errorf("unsure = %v", got.Unsure)
	}
}

func TestAPIReachCutPrefersAnExplicitLocalNetworkAlias(t *testing.T) {
	t.Parallel()
	r := &APIReach{
		Enabled: true, LocalNetwork: "10.1.0.0/24",
		Nodes: []NodeReach{node("pve1", "10.0.0.5", "10.0.0.5/24")},
	}
	got := r.Cut(addrs("10.0.0.20", "10.1.0.9"))
	if len(got.Cut) != 1 || got.Cut[0].String() != "10.0.0.20" {
		t.Errorf("cut = %v, want the node's own network cut once the alias moves it", got.Cut)
	}
}

func TestAPIReachCutHandlesIPv6(t *testing.T) {
	t.Parallel()
	r := &APIReach{Enabled: true, Nodes: []NodeReach{node("pve1", "2001:db8:1::5", "2001:db8:1::5/64", "10.0.0.5/24")}}
	got := r.Cut(addrs("2001:db8:1::77", "2001:db8:2::77", "10.0.0.9"))
	if len(got.Cut) != 1 || got.Cut[0].String() != "2001:db8:2::77" {
		t.Errorf("cut = %v", got.Cut)
	}
}

func TestAPIReachCutDoesNotLetAnIPv4NetworkHoldAnIPv6Address(t *testing.T) {
	t.Parallel()
	r := &APIReach{Enabled: true, Nodes: []NodeReach{node("pve1", "10.0.0.5", "10.0.0.5/24")}}
	got := r.Cut(addrs("2001:db8::9"))
	if len(got.Cut) != 1 {
		t.Errorf("cut = %v", got.Cut)
	}
}

func TestAPIReachCutChecksEveryNodeWithItsOwnNetwork(t *testing.T) {
	t.Parallel()
	r := &APIReach{Enabled: true, Nodes: []NodeReach{
		node("pve1", "10.0.0.5", "10.0.0.5/24", "10.9.0.5/24"),
		node("pve2", "10.0.1.5", "10.0.1.5/24", "10.9.1.5/24"),
	}}
	got := r.Cut(addrs("10.0.0.20", "10.0.1.20"))
	if len(got.Cut) != 2 {
		t.Errorf("cut = %v, want both, each on one node's network only", got.Cut)
	}
	if got := r.Cut(addrs("10.9.0.7")); len(got.Cut) != 1 {
		t.Errorf("a second interface that holds no node address is no local network: %v", got.Cut)
	}
}

func TestAPIReachCutSaysWhenTheLocalNetworkIsUnknown(t *testing.T) {
	t.Parallel()
	for name, n := range map[string]NodeReach{
		"no address":       {Node: "pve1", Interfaces: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}},
		"no interface":     {Node: "pve1", Address: netip.MustParseAddr("10.0.0.5")},
		"unrelated ifaces": node("pve1", "10.0.0.5", "192.168.0.5/24"),
	} {
		r := &APIReach{Enabled: true, Nodes: []NodeReach{n}}
		got := r.Cut(addrs("10.7.0.9"))
		if len(got.Cut) != 0 || len(got.Unsure) != 1 || !strings.Contains(got.Unsure[0], "node pve1") {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestAPIReachCutIsSilentAboutWhatItCouldNotReadWhenNothingNeedsIt(t *testing.T) {
	t.Parallel()
	r := &APIReach{Enabled: true, Unread: []string{"the IPSet management"}, Nodes: []NodeReach{node("pve1", "10.0.0.5", "10.0.0.5/24")}}
	if got := r.Cut(addrs("10.0.0.7")); len(got.Cut) != 0 || len(got.Unsure) != 0 {
		t.Errorf("%+v", got)
	}
	if got := r.Cut(addrs("10.5.0.7")); len(got.Unsure) != 1 {
		t.Errorf("%+v", got)
	}
}

func TestLockoutMessageNamesTheAddressesAndTheFix(t *testing.T) {
	t.Parallel()
	if LockoutMessage(nil) != "" || UnsureMessage(nil) != "" {
		t.Error("a message without a finding")
	}
	msg := LockoutMessage(addrs("10.1.0.9", "10.1.0.10"))
	if !strings.Contains(msg, "cut off 10.1.0.9, 10.1.0.10") || !strings.HasSuffix(msg, "Add 10.1.0.9, 10.1.0.10 to the datacenter firewall's IPSet management.") {
		t.Errorf("message = %q", msg)
	}
}

func fakeFirewall(answers map[string]string) Getter {
	return func(_ context.Context, path string, out any) error {
		body, ok := answers[path]
		if !ok {
			return errors.New("500 no such " + path)
		}
		return json.Unmarshal([]byte(body), out)
	}
}

func TestReadAPIReachReadsTheFirewallsOfEveryNode(t *testing.T) {
	t.Parallel()
	r, err := ReadAPIReach(context.Background(), fakeFirewall(map[string]string{
		"/cluster/firewall/options":          `{"enable":1}`,
		"/cluster/firewall/aliases":          `[{"name":"local_network","cidr":"10.0.0.0/22"}]`,
		"/cluster/firewall/ipset":            `[{"name":"management"}]`,
		"/cluster/firewall/ipset/management": `[{"cidr":"10.2.0.0/24"},{"cidr":"10.2.0.9","nomatch":1}]`,
		"/nodes":                             `[{"node":"pve1"},{"node":"pve2"},{"node":"pve3"}]`,
		"/cluster/status":                    `[{"type":"cluster","name":"c"},{"type":"node","name":"pve1","ip":"10.0.0.5"},{"type":"node","name":"pve2","ip":"10.0.0.6"},{"type":"node","name":"pve3","ip":"10.0.0.7"}]`,
		"/nodes/pve1/firewall/options":       `{}`,
		"/nodes/pve1/network":                `[{"iface":"vmbr0","cidr":"10.0.0.5/24"},{"iface":"vmbr1","cidr6":"2001:db8::5/64"},{"iface":"eno1"}]`,
		"/nodes/pve2/firewall/options":       `{"enable":0}`,
		"/nodes/pve3/firewall/options":       `{"enable":1}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Enabled || r.LocalNetwork != "10.0.0.0/22" || len(r.Management) != 2 || !r.Management[1].NoMatch {
		t.Errorf("reach = %+v", r)
	}
	// pve2's firewall is off, and pve3's interfaces cannot be read.
	if len(r.Nodes) != 2 || r.Nodes[0].Node != "pve1" || len(r.Nodes[0].Interfaces) != 2 || r.Nodes[0].Address.String() != "10.0.0.5" {
		t.Errorf("nodes = %+v", r.Nodes)
	}
	if len(r.Unread) != 1 || !strings.Contains(r.Unread[0], "pve3") {
		t.Errorf("unread = %v", r.Unread)
	}
}

func TestReadAPIReachFailsWithoutTheDatacenterOptions(t *testing.T) {
	t.Parallel()
	if _, err := ReadAPIReach(context.Background(), fakeFirewall(nil)); err == nil {
		t.Error("read without the options")
	}
}

func TestReadAPIReachTakesAMissingManagementIPSetAsEmpty(t *testing.T) {
	t.Parallel()
	r, err := ReadAPIReach(context.Background(), fakeFirewall(map[string]string{
		"/cluster/firewall/options": `{"enable":0}`,
		"/cluster/firewall/aliases": `[]`,
		"/cluster/firewall/ipset":   `[]`,
		"/nodes":                    `[]`,
		"/cluster/status":           `[]`,
	}))
	if err != nil || r.Enabled || len(r.Management) != 0 || len(r.Unread) != 0 {
		t.Errorf("%+v, %v", r, err)
	}
}
