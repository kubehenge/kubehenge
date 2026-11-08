package ui

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// The search for a new cluster's addresses starts this far into the bridge's network, past the
	// addresses networks often keep for routers, switches and servers. On a network of no more than
	// 64 addresses it starts at the first.
	freeStart = 20
	// The probes come from the management cluster, so a user may not have the interface scan a
	// whole network: a suggestion probes at most probeMost addresses, probeWindow at once, and a
	// check at most checkMost of those typed.
	probeMost   = 256
	probeWindow = 32
	checkMost   = 64
	nodesMost   = 64
	// pointToPoint is the prefix length from which a network has no network or broadcast address,
	// and pointToPointIPv6 the one from which an IPv6 network has no subnet-router anycast address.
	pointToPoint     = 31
	pointToPointIPv6 = 127
	// smallHostBits is the host bits of the smallest network the search starts past freeStart in.
	smallHostBits = 7
	byteBits      = 8
)

// The names of the address families in a request, as the cluster's ipFamilies variable has them.
const (
	familyNameIPv4 = "IPv4"
	familyNameIPv6 = "IPv6"
)

type freeRequest struct {
	VLAN int `json:"vlan"`
	// Nodes is how many node addresses the cluster needs. With none, only Check is probed.
	Nodes int `json:"nodes"`
	// Check are addresses, ranges or networks typed. Those on the bridge's networks are probed.
	Check []string `json:"check"`
	// Families are the address families to search, "IPv4" and "IPv6". Empty means IPv4.
	Families []string `json:"families"`
}

// freeView is a run of free addresses on a bridge's network for a new cluster, the first for its
// control-plane endpoint and the rest for its nodes, and the addresses checked that a host answers
// on. Network is empty where the node reports no network on the bridge or the cluster takes a VLAN
// of it: then nothing is probed. The IPv6 fields are the same for the bridge's IPv6 network.
type freeView struct {
	Network    string   `json:"network,omitempty"`
	Endpoint   string   `json:"endpoint,omitempty"`
	Addresses  string   `json:"addresses,omitempty"`
	Network6   string   `json:"network6,omitempty"`
	Endpoint6  string   `json:"endpoint6,omitempty"`
	Addresses6 string   `json:"addresses6,omitempty"`
	Answering  []string `json:"answering"`
}

// bridgeFamily is a node's network on a bridge in one address family, and the addresses on it
// spoken for.
type bridgeFamily struct {
	network  netip.Prefix
	reserved [][2]netip.Addr
}

// wantedFamilies tells which families a request asks for. An unknown name is an error.
func wantedFamilies(names []string) (bool, bool, error) {
	if len(names) == 0 {
		return true, false, nil
	}
	var want4, want6 bool
	for _, n := range names {
		switch n {
		case familyNameIPv4:
			want4 = true
		case familyNameIPv6:
			want6 = true
		default:
			return false, false, fmt.Errorf("families: %q is neither %s nor %s", n, familyNameIPv4, familyNameIPv6)
		}
	}
	return want4, want6, nil
}

// freeAddresses suggests addresses for a new cluster on a bridge that no other cluster holds and no
// host answers on, and tells which of the addresses typed a host answers on. It is advice: the
// controller refuses only addresses another cluster holds, as it cannot see the network.
func (s *Server) freeAddresses(w http.ResponseWriter, r *http.Request, u user) {
	var req freeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Nodes < 0 || req.Nodes > nodesMost {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("nodes: 0 to %d", nodesMost))
		return
	}
	want4, want6, err := wantedFamilies(req.Families)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	may, err := s.placesClusters(ctx, u)
	if err != nil {
		s.proxmoxError(w, r, err)
		return
	}
	if !may {
		writeError(w, http.StatusForbidden, fmt.Sprintf("%s needs %s on %s", u.ticket.Username, privChange, s.workloadPoolPath()))
		return
	}
	bridge := r.PathValue("bridge")
	view := freeView{Answering: []string{}}
	v4, v6, err := s.bridgeNetworks(ctx, u, r.PathValue("node"), bridge)
	if err != nil {
		s.proxmoxError(w, r, err)
		return
	}
	if !want4 {
		v4 = bridgeFamily{}
	}
	if !want6 {
		v6 = bridgeFamily{}
	}
	if (!v4.network.IsValid() && !v6.network.IsValid()) || req.VLAN != 0 {
		writeJSON(w, http.StatusOK, view)
		return
	}
	taken, err := s.takenAddresses(ctx, bridge, req.VLAN)
	if err != nil {
		s.Log.Error("reading the clusters' addresses", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the other clusters' addresses")
		return
	}
	var check []netip.Addr
	for _, f := range []bridgeFamily{v4, v6} {
		if f.network.IsValid() {
			check = append(check, checked(req.Check, f.network)...)
		}
	}
	answering := s.probe(ctx, check)
	for _, a := range slices.SortedFunc(maps.Keys(answering), netip.Addr.Compare) {
		view.Answering = append(view.Answering, a.String())
	}
	if v4.network.IsValid() {
		view.Network = v4.network.String()
		view.Endpoint, view.Addresses = s.suggest(ctx, v4, taken, req.Nodes)
	}
	if v6.network.IsValid() {
		view.Network6 = v6.network.String()
		view.Endpoint6, view.Addresses6 = s.suggest(ctx, v6, taken, req.Nodes)
	}
	writeJSON(w, http.StatusOK, view)
}

// suggest is a free endpoint and a free run of node addresses on a family's network, empty where
// none is wanted or none is free.
func (s *Server) suggest(ctx context.Context, f bridgeFamily, taken [][2]netip.Addr, nodes int) (string, string) {
	if nodes == 0 {
		return "", ""
	}
	run := s.freeRun(ctx, f.network, append(slices.Clone(taken), f.reserved...), nodes+1)
	if run == nil {
		return "", ""
	}
	addresses := run[1].String()
	if last := run[len(run)-1]; last != run[1] {
		addresses += "-" + last.String()
	}
	return run[0].String(), addresses
}

// bridgeNetworks are the networks a node reports on a bridge, IPv4 and IPv6, and the addresses on
// them spoken for: the node's own and the gateway's. A family is no network where the node has no
// address there.
func (s *Server) bridgeNetworks(ctx context.Context, u user, node, bridge string) (bridgeFamily, bridgeFamily, error) {
	bridges, err := s.Accounts.Bridges(ctx, u.ticket, node)
	if err != nil {
		return bridgeFamily{}, bridgeFamily{}, fmt.Errorf("reading the bridges of %s: %w", node, err)
	}
	var v4, v6 bridgeFamily
	for i := range bridges {
		b := &bridges[i]
		if b.Bridge != bridge {
			continue
		}
		if f, ok := familyOn(b.CIDR, b.Gateway, true); ok && !v4.network.IsValid() {
			v4 = f
		}
		if f, ok := familyOn(b.CIDR6, b.Gateway6, false); ok && !v6.network.IsValid() {
			v6 = f
		}
	}
	return v4, v6, nil
}

// familyOn is a node's network of one family on a bridge from its own address with the prefix
// length and its gateway.
func familyOn(cidr, gateway string, is4 bool) (bridgeFamily, bool) {
	own, ok := ownAddress(cidr, is4)
	if !ok {
		return bridgeFamily{}, false
	}
	reserved := [][2]netip.Addr{{own.Addr(), own.Addr()}}
	if gw, err := netip.ParseAddr(gateway); err == nil {
		reserved = append(reserved, [2]netip.Addr{gw, gw})
	}
	return bridgeFamily{network: own.Masked(), reserved: reserved}, true
}

// ownAddress is a node's address of one family on a bridge with its prefix length, like
// 192.0.2.5/24 or 2001:db8::5/64.
func ownAddress(cidr string, is4 bool) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(cidr)
	return p, err == nil && p.Addr().Is4() == is4 && !p.Addr().Is4In6()
}

// takenAddresses are the addresses Kubehenge's clusters hold on a bridge and VLAN. They are read
// with the interface's own account: the controller refuses the addresses of clusters the user
// cannot see too.
func (s *Server) takenAddresses(ctx context.Context, bridge string, vlan int) ([][2]netip.Addr, error) {
	kube := s.currentKube()
	if kube == nil {
		return nil, nil
	}
	var list clusterv1.ClusterList
	if err := kube.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	return provisioner.TakenAddresses(list.Items, bridge, vlan), nil
}

// freeRun is the first run of n consecutive addresses on the network, from freeStart on, that is
// none of taken and on which no host answers. Nil when the first probeMost addresses hold none.
func (s *Server) freeRun(ctx context.Context, network netip.Prefix, taken [][2]netip.Addr, n int) []netip.Addr {
	first, last := hosts(network)
	start := first
	if network.Addr().BitLen()-network.Bits() >= smallHostBits {
		start = advance(network.Addr(), freeStart)
	}
	var candidates []netip.Addr
	for a := start; a.IsValid() && a.Compare(last) <= 0 && len(candidates) < probeMost; a = a.Next() {
		if !slices.ContainsFunc(taken, func(t [2]netip.Addr) bool { return t[0].Compare(a) <= 0 && a.Compare(t[1]) <= 0 }) {
			candidates = append(candidates, a)
		}
	}
	var run []netip.Addr
	for window := range slices.Chunk(candidates, probeWindow) {
		answering := s.probe(ctx, window)
		for _, a := range window {
			if answering[a] || (len(run) > 0 && run[len(run)-1].Next() != a) {
				run = run[:0]
			}
			if answering[a] {
				continue
			}
			run = append(run, a)
			if len(run) == n {
				return run
			}
		}
	}
	return nil
}

// checked are the addresses of the entries that lie on the network, at most checkMost. Entries
// that don't read are left to the form's own checks.
func checked(entries []string, network netip.Prefix) []netip.Addr {
	first, last := hosts(network)
	var out []netip.Addr
	for _, e := range entries {
		spans, err := provisioner.AddressSpans([]string{e})
		if err != nil {
			continue
		}
		from, to := spans[0][0], spans[0][1]
		if from.Less(first) {
			from = first
		}
		if last.Less(to) {
			to = last
		}
		for a := from; a.IsValid() && a.Compare(to) <= 0; a = a.Next() {
			if len(out) == checkMost {
				return out
			}
			out = append(out, a)
		}
	}
	return out
}

// hosts are the first and last address of a network a host may take: all but its network and
// broadcast address in IPv4, and all but the subnet-router anycast address in IPv6.
func hosts(network netip.Prefix) (netip.Addr, netip.Addr) {
	first, last := network.Addr(), lastAddr(network)
	switch {
	case network.Addr().Is4() && network.Bits() < pointToPoint:
		return first.Next(), last.Prev()
	case network.Addr().Is6() && network.Bits() < pointToPointIPv6:
		return first.Next(), last
	}
	return first, last
}

// lastAddr is the last address of a network: its host bits all set.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := p.Bits(); i < len(b)*byteBits; i++ {
		b[i/byteBits] |= 1 << (byteBits - 1 - i%byteBits)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// advance is the address n past a.
func advance(a netip.Addr, n int) netip.Addr {
	for ; n > 0 && a.IsValid(); n-- {
		a = a.Next()
	}
	return a
}

// probe tells which of the addresses a host answers on.
func (s *Server) probe(ctx context.Context, addrs []netip.Addr) map[netip.Addr]bool {
	if len(addrs) == 0 {
		return nil
	}
	if s.Probe != nil {
		return s.Probe(ctx, addrs)
	}
	return probeAddresses(ctx, addrs, s.Log)
}
