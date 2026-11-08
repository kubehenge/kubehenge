package pve

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

// APIReach is who the Proxmox VE firewalls let in on the API port. With the datacenter firewall on,
// each node's own firewall (on unless its enable option says otherwise) accepts port 8006 only from
// the node's local network and the datacenter IPSet management. pve-firewall takes the local network
// from the alias local_network where the datacenter defines one. Otherwise it is the route that holds
// the node's own address (PVE::Firewall::local_network), and the nftables firewall takes the
// network of each interface that holds a host address (proxmox-ve-config, HostConfig::management_ips).
// Both are the node's own address with its interface's prefix as far as the API shows.
type APIReach struct {
	// Enabled is the datacenter firewall's enable option.
	Enabled bool
	// LocalNetwork is the datacenter's local_network alias, where it defines one.
	LocalNetwork string
	// Management are the entries of the IPSet management.
	Management []IPSetEntry
	// Aliases are the datacenter's aliases by name.
	Aliases map[string]string
	// Nodes are the nodes whose firewall is on.
	Nodes []NodeReach
	// Unread names what could not be read, so that the answer says it cannot be sure.
	Unread []string
}

// IPSetEntry is one line of an IPSet. Its CIDR is an address, a network or the name of an alias.
type IPSetEntry struct {
	CIDR    string
	NoMatch bool
}

// NodeReach is what a node's firewall takes as its local network.
type NodeReach struct {
	Node string
	// Address is the node's own address in /cluster/status, invalid where the cluster reports none.
	Address netip.Addr
	// Interfaces are the prefixes of all the node's interfaces.
	Interfaces []netip.Prefix
}

// Reach is the verdict of APIReach.Cut.
type Reach struct {
	// Cut are the addresses a node's firewall refuses on the API port, in the order asked.
	Cut []netip.Addr
	// Unsure names what stops the answer from being certain about the other addresses.
	Unsure []string
}

// APIReach reads the firewalls' settings with the user's session.
func (a *Accounts) APIReach(ctx context.Context, t *Ticket) (*APIReach, error) {
	return ReadAPIReach(ctx, func(ctx context.Context, path string, out any) error { return a.get(ctx, t, path, out) })
}

// ReadAPIReach reads the firewalls' settings with get. The datacenter's options must be readable.
// Anything else it cannot read is named in Unread. Every read needs Sys.Audit, on / or on the node.
func ReadAPIReach(ctx context.Context, get Getter) (*APIReach, error) {
	var options struct {
		Enable flag `json:"enable"`
	}
	if err := get(ctx, "/cluster/firewall/options", &options); err != nil {
		return nil, fmt.Errorf("reading the datacenter firewall's options: %w", err)
	}
	r := &APIReach{Enabled: bool(options.Enable), Aliases: map[string]string{}}

	var aliases []struct {
		Name string `json:"name"`
		CIDR string `json:"cidr"`
	}
	if err := get(ctx, "/cluster/firewall/aliases", &aliases); err != nil {
		r.Unread = append(r.Unread, "the datacenter firewall's aliases")
	}
	for _, a := range aliases {
		r.Aliases[a.Name] = a.CIDR
	}
	r.LocalNetwork = r.Aliases["local_network"]

	var entries []struct {
		CIDR    string `json:"cidr"`
		NoMatch flag   `json:"nomatch"`
	}
	// Reading an IPSet that does not exist is an error, so the list says whether management does.
	var sets []struct {
		Name string `json:"name"`
	}
	hasManagement := false
	if err := get(ctx, "/cluster/firewall/ipset", &sets); err != nil {
		r.Unread = append(r.Unread, "the datacenter firewall's IPSets")
	}
	for _, s := range sets {
		hasManagement = hasManagement || s.Name == "management"
	}
	if hasManagement {
		if err := get(ctx, "/cluster/firewall/ipset/management", &entries); err != nil {
			r.Unread = append(r.Unread, "the IPSet management")
		}
	}
	for _, e := range entries {
		r.Management = append(r.Management, IPSetEntry{CIDR: e.CIDR, NoMatch: bool(e.NoMatch)})
	}

	r.readNodes(ctx, get)
	return r, nil
}

func (r *APIReach) readNodes(ctx context.Context, get Getter) {
	var status []struct {
		Type string `json:"type"`
		Name string `json:"name"`
		IP   string `json:"ip"`
	}
	var nodes []struct {
		Node string `json:"node"`
	}
	if err := get(ctx, "/nodes", &nodes); err != nil {
		r.Unread = append(r.Unread, "the list of nodes")
		return
	}
	// A node on its own reports no address, and its local network cannot then be told.
	if err := get(ctx, "/cluster/status", &status); err != nil {
		r.Unread = append(r.Unread, "the nodes' addresses")
	}
	addresses := map[string]netip.Addr{}
	for _, s := range status {
		if ip, err := netip.ParseAddr(s.IP); s.Type == "node" && err == nil {
			addresses[s.Name] = ip
		}
	}
	for _, n := range nodes {
		escaped := url.PathEscape(n.Node)
		var options struct {
			Enable *flag `json:"enable"`
		}
		// A node without the option has its firewall on.
		if err := get(ctx, "/nodes/"+escaped+"/firewall/options", &options); err == nil && options.Enable != nil && !*options.Enable {
			continue
		}
		reach := NodeReach{Node: n.Node, Address: addresses[n.Node]}
		var ifaces []struct {
			CIDR  string `json:"cidr"`
			CIDR6 string `json:"cidr6"`
		}
		if err := get(ctx, "/nodes/"+escaped+"/network", &ifaces); err != nil {
			r.Unread = append(r.Unread, "the interfaces of node "+n.Node)
		}
		for _, i := range ifaces {
			for _, text := range [...]string{i.CIDR, i.CIDR6} {
				if p, err := netip.ParsePrefix(text); err == nil {
					reach.Interfaces = append(reach.Interfaces, p.Masked())
				}
			}
		}
		r.Nodes = append(r.Nodes, reach)
	}
}

// Cut tells which of addrs the firewalls refuse on the API port. An address is let in by a node's
// local network or by the IPSet management. An address of the other family than the node's own is
// let in where an interface of the node holds it, since the nftables firewall takes every host
// address' network and the iptables one cannot be told from the API.
func (r *APIReach) Cut(addrs []netip.Addr) Reach {
	var out Reach
	unsure := map[string]bool{}
	for _, a := range addrs {
		in, unresolved := r.inManagement(a)
		if in {
			continue
		}
		cut, allLocal := false, true
		for i := range r.Nodes {
			n := &r.Nodes[i]
			local, known := r.local(n, a)
			switch {
			case local:
			case known:
				cut, allLocal = true, false
			default:
				allLocal = false
				unsure["the local network of node "+n.Node] = true
			}
		}
		if allLocal {
			continue
		}
		if cut {
			out.Cut = append(out.Cut, a)
		}
		// Whatever could not be read might have let the address in.
		for _, name := range r.Unread {
			unsure[name] = true
		}
		if unresolved != "" {
			unsure[unresolved] = true
		}
	}
	out.Unsure = slices.Sorted(func(yield func(string) bool) {
		for k := range unsure {
			if !yield(k) {
				return
			}
		}
	})
	return out
}

// local tells whether node n's local network holds a, and whether that is known.
func (r *APIReach) local(n *NodeReach, a netip.Addr) (inside, known bool) {
	if r.LocalNetwork != "" {
		p, ok := parseSource(r.LocalNetwork)
		return ok && p.Contains(a), ok
	}
	if !n.Address.IsValid() {
		return false, false
	}
	found := false
	for _, p := range n.Interfaces {
		if p.Contains(n.Address) {
			found = true
			if p.Contains(a) {
				return true, true
			}
		}
	}
	if !found {
		return false, false
	}
	if n.Address.Is4() != a.Is4() {
		return slices.ContainsFunc(n.Interfaces, func(p netip.Prefix) bool { return p.Contains(a) }), true
	}
	return false, true
}

// inManagement tells whether the IPSet management holds a. unresolved names an entry that names an
// alias that is not there, which could hold a.
func (r *APIReach) inManagement(a netip.Addr) (in bool, unresolved string) {
	matched, excluded := false, false
	for _, e := range r.Management {
		p, ok := parseSource(e.CIDR)
		if !ok {
			name := strings.TrimPrefix(e.CIDR, "dc/")
			if p, ok = parseSource(r.Aliases[name]); !ok {
				unresolved = "the IPSet management entry " + e.CIDR
				continue
			}
		}
		switch {
		case !p.Contains(a):
		case e.NoMatch:
			excluded = true
		default:
			matched = true
		}
	}
	in = matched && !excluded
	if in || excluded {
		unresolved = ""
	}
	return in, unresolved
}

// parseSource reads an address or a network as the network it covers.
func parseSource(text string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(strings.TrimSpace(text)); err == nil {
		return p.Masked(), true
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(text)); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

// LockoutMessage words the finding for the addresses cut off, and the fix. It is the same text in the
// setup's pre-flight and in the controller's refusal. It is empty where nothing is cut.
func LockoutMessage(cut []netip.Addr) string {
	if len(cut) == 0 {
		return ""
	}
	texts := make([]string, len(cut))
	for i, a := range cut {
		texts[i] = a.String()
	}
	list := strings.Join(texts, ", ")
	return fmt.Sprintf("The Proxmox VE nodes' firewall lets the API in only from each node's local network and the IPSet management, so it would cut off %s. Add %s to the datacenter firewall's IPSet management.", list, list)
}

// UnsureMessage words what could not be told. It is empty where nothing is unsure.
func UnsureMessage(unsure []string) string {
	if len(unsure) == 0 {
		return ""
	}
	return fmt.Sprintf("Could not tell whether the Proxmox VE nodes' firewall lets the rest of the management side in, for want of %s. Check that its addresses are in the datacenter firewall's IPSet management.", strings.Join(unsure, ", "))
}
