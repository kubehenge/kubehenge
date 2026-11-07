package provisioner

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// The address families a cluster's network.ipFamilies name, as Kubernetes writes them.
const (
	familyIPv4 = "IPv4"
	familyIPv6 = "IPv6"
	// networkVariable is the cluster variable of the nodes' network.
	networkVariable = "network"
)

// nodeNetwork is a cluster's network variable. The IPv4 network is in its own fields and the IPv6
// one under ipv6. Each is there exactly when IPFamilies hold its family.
type nodeNetwork struct {
	Bridge     string   `json:"bridge"`
	VLAN       int      `json:"vlan"`
	IPFamilies []string `json:"ipFamilies"`
	Gateway    string   `json:"gateway"`
	Prefix     int      `json:"prefix"`
	Addrs      []string `json:"addresses"`
	// IPv6 is the nodes' IPv6 network, nil when the cluster has none.
	IPv6       *familyNetwork `json:"ipv6"`
	DNSServers []string       `json:"dnsServers"`
	NTP        []string       `json:"ntp"`
	// MTU and Queues are the nodes' interface options, nil when unset.
	MTU    *int `json:"mtu"`
	Queues *int `json:"queues"`
}

// familyNetwork is the nodes' network of one address family.
type familyNetwork struct {
	Addrs   []string `json:"addresses"`
	Prefix  int      `json:"prefix"`
	Gateway string   `json:"gateway"`
}

// families are the cluster's address families, the primary first. A cluster that names none has
// IPv4 alone, the variable's default.
func (n *nodeNetwork) families() []string {
	if len(n.IPFamilies) == 0 {
		return []string{familyIPv4}
	}
	return n.IPFamilies
}

// has reports whether the cluster has the address family.
func (n *nodeNetwork) has(family string) bool {
	return slices.Contains(n.families(), family)
}

// of is the nodes' network of a family, nil where the variable holds none.
func (n *nodeNetwork) of(family string) *familyNetwork {
	if family == familyIPv6 {
		return n.IPv6
	}
	if n.Gateway == "" && n.Prefix == 0 && len(n.Addrs) == 0 {
		return nil
	}
	return &familyNetwork{Addrs: n.Addrs, Prefix: n.Prefix, Gateway: n.Gateway}
}

// familyPath is where a family's settings are in the network variable.
func familyPath(family string) string {
	if family == familyIPv6 {
		return "network.ipv6"
	}
	return networkVariable
}

// familyOf is the address family of an address.
func familyOf(a netip.Addr) string {
	if a.Unmap().Is4() {
		return familyIPv4
	}
	return familyIPv6
}

// nodeSubnet is the nodes' network of one family: its prefix and gateway.
type nodeSubnet struct {
	family  string
	prefix  netip.Prefix
	gateway netip.Addr
}

// subnets are the nodes' networks of each of the cluster's families, the primary first.
func (n *nodeNetwork) subnets() ([]nodeSubnet, error) {
	out := make([]nodeSubnet, 0, len(n.families()))
	for _, family := range n.families() {
		f := n.of(family)
		path := familyPath(family)
		if f == nil {
			return nil, fmt.Errorf("%s: the cluster's families hold %s but the network has no %s settings", path, family, family)
		}
		gateway, err := netip.ParseAddr(f.Gateway)
		if err != nil || familyOf(gateway) != family {
			return nil, fmt.Errorf("%s.gateway: %q is no %s address", path, f.Gateway, family)
		}
		prefix, err := gateway.Prefix(f.Prefix)
		if err != nil {
			return nil, fmt.Errorf("%s.prefix: %w", path, err)
		}
		out = append(out, nodeSubnet{family: family, prefix: prefix, gateway: gateway})
	}
	return out, nil
}

// subnetOf is the cluster's network of an address's family, if the cluster has that family.
func subnetOf(subnets []nodeSubnet, a netip.Addr) (nodeSubnet, bool) {
	i := slices.IndexFunc(subnets, func(s nodeSubnet) bool { return s.family == familyOf(a) })
	if i < 0 {
		return nodeSubnet{}, false
	}
	return subnets[i], true
}

// nodeSubnets are the cluster's networks of each of its families, the primary first.
func nodeSubnets(cluster *clusterv1.Cluster) ([]nodeSubnet, error) {
	network, err := variable[nodeNetwork](cluster, networkVariable)
	if err != nil {
		return nil, err
	}
	return network.subnets()
}

// clusterFamilies are a cluster's address families, IPv4 alone where its network does not read.
func clusterFamilies(cluster *clusterv1.Cluster) []string {
	network, _, err := optionalVariable[nodeNetwork](cluster, networkVariable)
	if err != nil {
		return []string{familyIPv4}
	}
	return network.families()
}

// lastAddress is a network's last address.
func lastAddress(p netip.Prefix) netip.Addr {
	first := p.Masked().Addr()
	b := first.AsSlice()
	for i := p.Bits(); i < first.BitLen(); i++ {
		b[i/byteBits] |= 1 << (byteBits - 1 - i%byteBits)
	}
	last, _ := netip.AddrFromSlice(b)
	return last
}

// byteBits are the bits of a byte of an address.
const byteBits = 8

// reservedAddresses are a network's addresses that no host answers at: an IPv4 network's own address
// and its broadcast, an IPv6 network's subnet-router anycast address. A Service given one would
// never be answered. A point-to-point network or a single address has none.
func reservedAddresses(p netip.Prefix) []netip.Addr {
	if p.Addr().Is4() {
		if p.Bits() >= pointToPointBits {
			return nil
		}
		return []netip.Addr{p.Masked().Addr(), lastAddress(p)}
	}
	if p.Bits() >= pointToPointBitsIPv6 {
		return nil
	}
	return []netip.Addr{p.Masked().Addr()}
}

// pointToPointBitsIPv6 is the prefix length from which an IPv6 network is a point-to-point link
// (RFC 6164) with no subnet-router anycast address.
const pointToPointBitsIPv6 = 127

// maxCount is where counts of addresses stop: an IPv6 network holds more than any cluster takes.
const maxCount = math.MaxInt32

// spanSize is how many addresses a span holds, at most maxCount.
func spanSize(s addressSpan) int {
	first, last := s.first.As16(), s.last.As16()
	hiF, hiL := binary.BigEndian.Uint64(first[:8]), binary.BigEndian.Uint64(last[:8])
	loF, loL := binary.BigEndian.Uint64(first[8:]), binary.BigEndian.Uint64(last[8:])
	if hiL != hiF {
		return maxCount
	}
	n := loL - loF
	if n >= maxCount {
		return maxCount
	}
	return int(n) + 1
}

// validateFamilies holds a cluster's network to its address families: addresses and DNS servers of
// its families, in the settings of their family, and the families it was created with.
func validateFamilies(old, c *clusterv1.Cluster) field.ErrorList {
	path := field.NewPath("spec", "topology", "variables").Key(networkVariable)
	network, set, err := optionalVariable[nodeNetwork](c, networkVariable)
	if err != nil || !set {
		// The class's schema refuses a missing or unreadable network.
		return nil
	}
	var errs field.ErrorList
	if old == nil || variableChanged(old, c, networkVariable) {
		for _, p := range familiesProblems(&network) {
			errs = append(errs, field.Invalid(path, nil, p))
		}
	}
	if old != nil {
		if was, _, err := optionalVariable[nodeNetwork](old, networkVariable); err == nil && !slices.Equal(was.families(), network.families()) {
			errs = append(errs, field.Forbidden(path.Child("ipFamilies"), "the address families are chosen when the cluster is created"))
		}
	}
	return errs
}

// cniOf is the CNI a cluster runs, by its cni variable or else its class's default, "" where neither
// names one.
func cniOf(c *clusterv1.Cluster, class *clusterv1.ClusterClass) string {
	cni, set, err := optionalVariable[string](c, "cni")
	if err != nil {
		return ""
	}
	if set {
		return cni
	}
	i := slices.IndexFunc(class.Spec.Variables, func(v clusterv1.ClusterClassVariable) bool { return v.Name == "cni" })
	if i < 0 || class.Spec.Variables[i].Schema.OpenAPIV3Schema.Default == nil ||
		json.Unmarshal(class.Spec.Variables[i].Schema.OpenAPIV3Schema.Default.Raw, &cni) != nil {
		return ""
	}
	return cni
}

// familiesProblems are where a cluster's network variable holds addresses in the other family's
// settings, and DNS servers that are no use to it. A server of a family the cluster lacks reaches
// none of its nodes, and a dual-stack cluster needs one of each.
func familiesProblems(n *nodeNetwork) []string {
	return append(addressFamilyProblems(n), dnsFamilyProblems(n)...)
}

// addressFamilyProblems are the node addresses in the other family's settings.
func addressFamilyProblems(n *nodeNetwork) []string {
	var problems []string
	for _, family := range []string{familyIPv4, familyIPv6} {
		f := n.of(family)
		if f == nil {
			continue
		}
		path := familyPath(family)
		for _, e := range f.Addrs {
			s, err := parseSpan(strings.TrimSpace(e))
			if err == nil && familyOf(s.first) != family {
				problems = append(problems, fmt.Sprintf("%s.addresses holds %s addresses, and %s is %s", path, family, e, familyOf(s.first)))
			}
		}
	}
	return problems
}

// dnsFamilyProblems are the DNS servers of a family the cluster lacks, and a family of a dual-stack
// cluster that none is of.
func dnsFamilyProblems(n *nodeNetwork) []string {
	var problems []string
	seen := map[string]bool{}
	for _, d := range n.DNSServers {
		a, err := netip.ParseAddr(strings.TrimSpace(d))
		if err != nil {
			problems = append(problems, fmt.Sprintf("DNS server %q is no IP address", d))
			continue
		}
		if !n.has(familyOf(a)) {
			problems = append(problems, fmt.Sprintf("DNS server %s is %s, which the cluster's nodes do not have", a, familyOf(a)))
		}
		seen[familyOf(a)] = true
	}
	if len(n.families()) == 2 && len(n.DNSServers) > 0 {
		for _, family := range n.families() {
			if !seen[family] {
				problems = append(problems, "a dual-stack cluster needs a DNS server of each family, and none is "+family)
			}
		}
	}
	return problems
}
