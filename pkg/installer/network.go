package installer

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// The kinds of network a container attaches to.
const (
	kindBridge = "bridge"
	kindOVS    = "Open vSwitch"
	kindVNet   = "VNet"
)

// Where a gateway was read, shown beside it.
const (
	gwFromVNet  = "the VNet's subnet"
	gwFromNode  = "this node's route on that network"
	gwFromGuess = "a guess: the node's gateway moved onto this subnet"
)

// Network is a bridge, an Open vSwitch bridge or an SDN VNet of the node.
type Network struct {
	Name  string
	Kind  string
	Zone  string
	Tag   string
	Alias string
	// Subnet is the IPv4 subnet of the network where it is known: the node's own address on a
	// bridge, the first subnet of a VNet.
	Subnet netip.Prefix
	// VLANAware is whether the network carries VLAN tags of its own.
	VLANAware bool
}

// Label is the network's line in the list the user picks from.
func (n *Network) Label() string {
	parts := []string{n.Name, n.Kind}
	if n.Zone != "" {
		parts = append(parts, "zone "+n.Zone)
	}
	if n.Tag != "" {
		parts = append(parts, "tag "+n.Tag)
	}
	if n.Subnet.IsValid() {
		parts = append(parts, "subnet "+n.Subnet.String())
	}
	if n.Alias != "" {
		parts = append(parts, n.Alias)
	}
	return strings.Join(parts, ", ")
}

// VLANOffered reports whether the container's VLAN is asked for on this network. A VNet is bound
// to its own tag unless it is VLAN aware.
func (n *Network) VLANOffered() bool { return n.Kind != kindVNet || n.VLANAware }

// networkNames lists the names of the networks.
func networkNames(list []Network) []string {
	names := make([]string, len(list))
	for i, n := range list {
		names[i] = n.Name
	}
	return names
}

// networks lists the bridges, Open vSwitch bridges and SDN VNets a container can attach to.
func (n *Node) networks(ctx context.Context) []Network {
	seen := map[string]bool{}
	var list []Network
	add := func(net Network) {
		if net.Name != "" && !seen[net.Name] {
			seen[net.Name] = true
			list = append(list, net)
		}
	}
	if out, err := n.run(ctx, "pvesh", "get", "/nodes/"+n.Hostname+"/network", "--output-format", "json"); err == nil {
		for _, m := range jsonList(out) {
			switch str(m, "type") {
			case "bridge":
				aware, _ := flagOn(m["bridge_vlan_aware"])
				add(Network{Name: str(m, "iface"), Kind: kindBridge, Subnet: n.bridgeAddress4(ctx, str(m, "iface")).Masked(), VLANAware: aware})
			case "OVSBridge":
				add(Network{Name: str(m, "iface"), Kind: kindOVS, Subnet: n.bridgeAddress4(ctx, str(m, "iface")).Masked(), VLANAware: true})
			}
		}
	}
	// Where SDN is not set up, the call fails and there are none.
	if out, err := n.run(ctx, "pvesh", "get", "/cluster/sdn/vnets", "--output-format", "json"); err == nil {
		for _, m := range jsonList(out) {
			aware, _ := flagOn(m["vlanaware"])
			subnet, _ := n.vnetSubnet(ctx, str(m, "vnet"))
			add(Network{Name: str(m, "vnet"), Kind: kindVNet, Zone: str(m, "zone"), Tag: str(m, "tag"), Alias: str(m, "alias"), Subnet: subnet.Masked(), VLANAware: aware})
		}
	}
	slices.SortFunc(list, func(a, b Network) int { return strings.Compare(a.Name, b.Name) })
	return list
}

// network returns the named network.
func (f *Facts) network(name string) (Network, bool) {
	for _, n := range f.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return Network{}, false
}

// vnetSubnet is the first IPv4 subnet of an SDN VNet and its gateway, from the SDN's own configuration.
func (n *Node) vnetSubnet(ctx context.Context, vnet string) (netip.Prefix, string) {
	out, err := n.run(ctx, "pvesh", "get", "/cluster/sdn/vnets/"+vnet+"/subnets", "--output-format", "json")
	if err != nil {
		return netip.Prefix{}, ""
	}
	for _, m := range jsonList(out) {
		p, perr := netip.ParsePrefix(subnetCIDR(m))
		if perr != nil || !p.Addr().Is4() {
			continue
		}
		gw := str(m, "gateway")
		if _, gerr := netip.ParseAddr(gw); gerr != nil {
			gw = ""
		}
		return p, gw
	}
	return netip.Prefix{}, ""
}

// subnetCIDR reads a subnet's range from the SDN's answer, which names it by "cidr" or by
// "network" and "mask".
func subnetCIDR(m map[string]any) string {
	if c := str(m, "cidr"); c != "" {
		return c
	}
	if network, mask := str(m, "network"), str(m, "mask"); network != "" && mask != "" {
		return network + "/" + mask
	}
	return ""
}

// networkDevice is the device the node asks the container's network through: the bridge or VNet
// when no VLAN is given, else the bridge's VLAN device where the node has one, and then empty. The
// prefix is the node's own address on it, invalid where it holds none.
func (n *Node) networkDevice(ctx context.Context, bridge, vlan string) (string, netip.Prefix) {
	if vlan == "" {
		return bridge, n.bridgeAddress4(ctx, bridge)
	}
	dev := bridge + "." + vlan
	if p := n.bridgeAddress4(ctx, dev); p.IsValid() {
		return dev, p
	}
	return "", netip.Prefix{}
}

// deviceGateway is the default gateway the node's routes give the device.
func (n *Node) deviceGateway(ctx context.Context, dev string) string {
	out, err := n.run(ctx, "ip", "-4", "route", "show", "default", "dev", dev)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		if m := viaDevRE.FindStringSubmatch(strings.TrimSpace(line)); len(m) > 2 && (m[2] == "" || m[2] == dev) {
			return m[1]
		}
	}
	return ""
}

// RefreshNetwork reads the subnet, the gateway, whether DHCP is likely, an address suggestion and
// the API candidates again for the network the container is going on.
func (s *Session) RefreshNetwork(ctx context.Context, bridge, vlan string) {
	s.Node.refreshNetwork(ctx, s.Facts, bridge, vlan)
}

func (n *Node) refreshNetwork(ctx context.Context, f *Facts, bridge, vlan string) {
	f.Prefix, f.Gateway, f.GatewayFrom, f.NodeDHCP, f.SuggestedIP, f.Device, f.NodeHolds = netip.Prefix{}, "", "", false, "", "", false
	f.SuggestedCheck = AddrCheck{}
	if net, ok := f.network(bridge); ok && net.Kind == kindVNet {
		n.readVNet(ctx, f, bridge)
	} else {
		n.readHostNetwork(ctx, f, bridge, vlan)
	}
	if f.Prefix.IsValid() && f.Device != "" {
		f.SuggestedIP, f.SuggestedCheck = n.SuggestAddress(ctx, f.Device, f)
	}
	dev := f.Device
	if dev == "" {
		dev = bridge
	}
	f.Candidates = n.APICandidates(ctx, f, dev)
}

// readVNet takes the subnet and gateway of a VNet from the SDN's own configuration.
func (n *Node) readVNet(ctx context.Context, f *Facts, vnet string) {
	f.Prefix, f.Gateway = n.vnetSubnet(ctx, vnet)
	if f.Gateway != "" {
		f.GatewayFrom = gwFromVNet
	}
	f.Device = vnet
}

// readHostNetwork takes the subnet, gateway and DHCP use of a network from the node's own address on it.
func (n *Node) readHostNetwork(ctx context.Context, f *Facts, bridge, vlan string) {
	f.Device, f.Prefix = n.networkDevice(ctx, bridge, vlan)
	f.NodeHolds = f.Prefix.IsValid()
	if !f.NodeHolds {
		return
	}
	f.NodeDHCP = n.bridgeIsDHCP(f.Device)
	if gw := n.deviceGateway(ctx, f.Device); gw != "" {
		f.Gateway, f.GatewayFrom = gw, gwFromNode
	} else if g, err := netip.ParseAddr(f.RouteGateway); err == nil && f.Prefix.Contains(g) {
		f.Gateway, f.GatewayFrom = f.RouteGateway, gwFromNode
	}
}

// GuessGateway is the gateway for an address that is not in the network the gateway was found for:
// the node's gateway with its network part replaced by the address's and its host part kept. It is
// empty where no such gateway can be formed.
func GuessGateway(addr netip.Prefix, known string) string {
	g, err := netip.ParseAddr(known)
	if err != nil || !g.Is4() || !addr.IsValid() {
		return ""
	}
	if addr.Contains(g) {
		return g.String()
	}
	mask := ^uint32(0) << (ipv4Bits - addr.Bits())
	gb, nb := g.As4(), addr.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(nb[:]) | binary.BigEndian.Uint32(gb[:])&^mask
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	guess := netip.AddrFrom4(out)
	if guess == addr.Addr() || !usableHost(addr, guess) {
		return ""
	}
	return guess.String()
}

// usableHost reports whether a is a host address of the subnet, neither its network nor its broadcast
// address. A /31 and a /32 have no such addresses.
func usableHost(p netip.Prefix, a netip.Addr) bool {
	if !p.Contains(a) {
		return false
	}
	if p.Bits() >= ipv4Bits-1 {
		return true
	}
	return a != p.Masked().Addr() && a != lastAddress(p)
}

// ParseAddress4 reads an IPv4 address typed with or without a prefix. The prefix of known fills one
// in where none is typed. The second result is false when the text is no address, or has no prefix
// and none is known.
func ParseAddress4(text string, known netip.Prefix) (netip.Prefix, bool) {
	if !strings.Contains(text, "/") {
		if !IsIPv4(text) || !known.IsValid() {
			return netip.Prefix{}, false
		}
		text = fmt.Sprintf("%s/%d", text, known.Bits())
	}
	if !IsIPv4CIDR(text) {
		return netip.Prefix{}, false
	}
	p, err := netip.ParsePrefix(text)
	return p, err == nil
}

// checkPair checks the address and the gateway against each other: the address is a host of its
// subnet, is not the gateway, and the gateway lies in the subnet.
func checkPair(addr netip.Prefix, gateway string) error {
	if !usableHost(addr, addr.Addr()) {
		return fmt.Errorf("%s is the network or broadcast address of %s, give a host address", addr.Addr(), addr.Masked())
	}
	g, err := netip.ParseAddr(gateway)
	if gateway == "" || err != nil {
		return nil //nolint:nilerr // an empty gateway is the caller's to judge, and a malformed one fails its own check.
	}
	switch {
	case g == addr.Addr():
		return fmt.Errorf("the address and the gateway are both %s", g)
	case !addr.Contains(g):
		return fmt.Errorf("gateway %s is outside %s", g, addr.Masked())
	case !usableHost(addr, g):
		return fmt.Errorf("gateway %s is the network or broadcast address of %s", g, addr.Masked())
	}
	return nil
}

// checkPair6 is checkPair for IPv6. A link-local gateway is allowed, a link-local address is not.
func checkPair6(addr netip.Prefix, gateway string) error {
	a := addr.Addr()
	if !a.IsGlobalUnicast() || a.IsLinkLocalUnicast() {
		return fmt.Errorf("%s is no global address, give one the router's prefix covers", a)
	}
	g, err := netip.ParseAddr(gateway)
	if gateway == "" || err != nil || g.IsLinkLocalUnicast() {
		return nil //nolint:nilerr // an empty or link-local gateway needs no subnet check, and a malformed one fails its own check.
	}
	switch {
	case g == a:
		return fmt.Errorf("the address and the gateway are both %s", g)
	case !addr.Contains(g):
		return fmt.Errorf("gateway %s is outside %s", g, addr.Masked())
	}
	return nil
}

// OutsideSubnet is the message for an address outside the subnet the node knows of the network, empty
// when the subnet is unknown or holds the address.
func OutsideSubnet(known, addr netip.Prefix) string {
	if !known.IsValid() || known.Contains(addr.Addr()) {
		return ""
	}
	return fmt.Sprintf("%s is outside %s, the subnet of this network", addr.Addr(), known.Masked())
}
