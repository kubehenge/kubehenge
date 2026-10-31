package ui

import (
	"context"
	"encoding/binary"
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
	// smallNetwork addresses it starts at the first.
	freeStart    = 20
	smallNetwork = 64
	// The probes come from the management cluster, so a user may not have the interface scan a
	// whole network: a suggestion probes at most probeMost addresses, probeWindow at once, and a
	// check at most checkMost of those typed.
	probeMost   = 256
	probeWindow = 32
	checkMost   = 64
	nodesMost   = 64
	// pointToPoint is the prefix length from which a network has no network or broadcast address.
	pointToPoint = 31
)

type freeRequest struct {
	VLAN int `json:"vlan"`
	// Nodes is how many node addresses the cluster needs. With none, only Check is probed.
	Nodes int `json:"nodes"`
	// Check are addresses, ranges or networks typed. Those on the bridge's network are probed.
	Check []string `json:"check"`
}

// freeView is a run of free addresses on a bridge's network for a new cluster, the first for its
// control-plane endpoint and the rest for its nodes, and the addresses checked that a host answers
// on. Network is empty where the node reports no network on the bridge or the cluster takes a VLAN
// of it: then nothing is probed.
type freeView struct {
	Network   string   `json:"network,omitempty"`
	Endpoint  string   `json:"endpoint,omitempty"`
	Addresses string   `json:"addresses,omitempty"`
	Answering []string `json:"answering"`
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
	network, reserved, err := s.bridgeNetwork(ctx, u, r.PathValue("node"), bridge)
	if err != nil {
		s.proxmoxError(w, r, err)
		return
	}
	if !network.IsValid() || req.VLAN != 0 {
		writeJSON(w, http.StatusOK, view)
		return
	}
	view.Network = network.String()
	taken, err := s.takenAddresses(ctx, bridge, req.VLAN)
	if err != nil {
		s.Log.Error("reading the clusters' addresses", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the other clusters' addresses")
		return
	}
	answering := s.probe(ctx, checked(req.Check, network))
	for _, a := range slices.SortedFunc(maps.Keys(answering), netip.Addr.Compare) {
		view.Answering = append(view.Answering, a.String())
	}
	if req.Nodes == 0 {
		writeJSON(w, http.StatusOK, view)
		return
	}
	if run := s.freeRun(ctx, network, append(taken, reserved...), req.Nodes+1); run != nil {
		view.Endpoint = run[0].String()
		view.Addresses = run[1].String()
		if last := run[len(run)-1]; last != run[1] {
			view.Addresses += "-" + last.String()
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// bridgeNetwork is the network a node reports on a bridge, and the addresses on it spoken for: the
// node's own and the gateway's. It is no network where the node has no address there.
func (s *Server) bridgeNetwork(ctx context.Context, u user, node, bridge string) (netip.Prefix, [][2]netip.Addr, error) {
	bridges, err := s.Accounts.Bridges(ctx, u.ticket, node)
	if err != nil {
		return netip.Prefix{}, nil, fmt.Errorf("reading the bridges of %s: %w", node, err)
	}
	for _, b := range bridges {
		own, ok := ownAddress(b.CIDR)
		if b.Bridge != bridge || !ok {
			continue
		}
		reserved := [][2]netip.Addr{{own.Addr(), own.Addr()}}
		if gw, err := netip.ParseAddr(b.Gateway); err == nil {
			reserved = append(reserved, [2]netip.Addr{gw, gw})
		}
		return own.Masked(), reserved, nil
	}
	return netip.Prefix{}, nil, nil
}

// ownAddress is a node's IPv4 address on a bridge with its prefix length, like 192.0.2.5/24.
func ownAddress(cidr string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(cidr)
	return p, err == nil && p.Addr().Is4()
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
	if addrInt(last)-addrInt(network.Addr()) >= smallNetwork {
		start = intAddr(addrInt(network.Addr()) + freeStart)
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
// broadcast address, where it has them.
func hosts(network netip.Prefix) (netip.Addr, netip.Addr) {
	first := network.Addr()
	last := intAddr(addrInt(first) | (uint32(1)<<(first.BitLen()-network.Bits()) - 1))
	if network.Bits() < pointToPoint {
		return first.Next(), last.Prev()
	}
	return first, last
}

func addrInt(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func intAddr(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
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
