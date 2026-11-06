package appliance

import (
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// What each management VM takes of its node and storage. They are the module's defaults, which the
// setup does not change.
const (
	vmMemoryMiB = 4096
	vmDiskGiB   = 32
	gib         = int64(1) << 30
	mib         = int64(1) << 20
)

// The names the module gives what it creates in Proxmox VE. They are its defaults, which the setup
// does not change, and a run that finds one taken fails deep in the apply log.
const moduleName = "kubehenge"

var modulePools = []string{"kubehenge-management", "kubehenge", "kubehenge-templates"}

// Inventory is what a preflight reads of Proxmox VE with the user's session. A nil map or slice
// means it could not be read, and the checks that need it are left out rather than failing.
type Inventory struct {
	Storages []pve.Storage
	Nodes    []pve.Node
	// Bridges are by node.
	Bridges map[string][]pve.Bridge
	// Networks are where Proxmox VE has each bridge the settings name, by its name. A bridge whose
	// VNet the user may not read is missing.
	Networks map[string]pve.Network
	// Pools, Roles, Users and Groups are the IDs that exist.
	Pools, Roles, Users, Groups map[string]bool
	// NodeAddresses are the Proxmox VE nodes' own addresses, by node.
	NodeAddresses map[string][]netip.Addr
	// Answering are the addresses a host answers on by ping or TCP.
	Answering map[netip.Addr]bool
	// ProxmoxURL is the Proxmox VE API address the management cluster's controllers will use.
	ProxmoxURL string
	// NextID is the datacenter's next-id option. Zero means it is unset, or the user may not read it.
	NextID pve.NextID
	// UsedVMIDs are the IDs of the VMs and containers the user sees. Nil means they could not be
	// read.
	UsedVMIDs map[int]bool
}

// Findings are what a preflight found. Errors stop the run, warnings do not. Each names the node,
// storage or object and the fix.
type Findings struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
	// PodRanges and ServiceRanges are the ranges the run will use, set or chosen.
	PodRanges     []string `json:"podRanges,omitempty"`
	ServiceRanges []string `json:"serviceRanges,omitempty"`
}

// moduleNames are the pools, roles, users and groups the module creates for the settings.
func (s *Settings) moduleNames() moduleObjects {
	roles := []string{"audit", "templates-audit", "allocate-space", "cluster-vms", "cluster-storage", "provisioner-pools", "provisioner-users", "provisioner-realm"}
	users := []string{"cluster-api", "provisioner"}
	groups := []string{"clusters"}
	if s.GivesClusterSysAudit() {
		groups = append(groups, "clusters-audit")
	}
	if s.ClusterDatastoreAllocate {
		roles = append(roles, "cluster-datastore")
	}
	if s.ProxmoxHA {
		roles, users = append(roles, "ha"), append(users, "ha")
	}
	if s.EtcdRestore {
		roles, users = append(roles, "etcd-restore"), append(users, "etcd-restore")
	}
	if s.RefreshesTemplates() {
		roles = append(roles, "template-builder-vms", "template-builder-files", "template-builder-network")
		users = append(users, "template-builder")
	}
	prefix := func(names []string, suffix string) []string {
		out := make([]string, len(names))
		for i, n := range names {
			out[i] = moduleName + "-" + n + suffix
		}
		return out
	}
	return moduleObjects{pools: modulePools, roles: prefix(roles, ""), users: prefix(users, "@pve"), groups: prefix(groups, "")}
}

// moduleObjects are the IDs of what the module creates, by kind.
type moduleObjects struct{ pools, roles, users, groups []string }

// vmsOn counts the management VMs each node gets: the nodes are taken in turn.
func (s *Settings) vmsOn() map[string]int {
	counts := map[string]int{}
	for i := range s.NodeAddresses {
		if len(s.Nodes) > 0 {
			counts[s.Nodes[i%len(s.Nodes)]]++
		}
	}
	return counts
}

// Preflight checks the settings against what Proxmox VE reports, before the module runs. owned tells
// that the module's state exists, so the objects and addresses it made are its own. The settings are
// valid, as Validate checked them.
func (s *Settings) Preflight(inv *Inventory, owned bool) Findings { //nolint:revive // owned picks which checks apply, not a mode of one
	f := Findings{Errors: []string{}, Warnings: []string{}}
	s.checkNodesExist(inv, &f)
	s.checkStorages(inv, &f)
	s.checkBridge(inv, &f)
	// The VMs of an earlier run hold the memory, disks and addresses already.
	if !owned {
		s.checkMemory(inv, &f)
		s.checkDiskSpace(inv, &f)
		s.checkAddressesFree(inv, &f)
		s.checkNamesFree(inv, &f)
		s.checkVMIDs(inv, &f)
	}
	s.warnPlacement(inv, &f)
	s.warnRanges(inv, &f)
	return f
}

func (s *Settings) checkNodesExist(inv *Inventory, f *Findings) {
	if inv.Nodes == nil {
		return
	}
	for _, name := range s.Nodes {
		i := slices.IndexFunc(inv.Nodes, func(n pve.Node) bool { return n.Node == name })
		switch {
		case i < 0:
			f.Errors = append(f.Errors, fmt.Sprintf("Node %s is not in the Proxmox VE cluster. Choose a node that is.", name))
		case !inv.Nodes[i].Online:
			f.Errors = append(f.Errors, fmt.Sprintf("Node %s is offline. Start it, or choose another node.", name))
		}
	}
}

// checkStorages checks the three storages on every chosen node: present and enabled there, active,
// with the content type the module puts there, and room for the VM disks.
func (s *Settings) checkStorages(inv *Inventory, f *Findings) {
	if inv.Storages == nil {
		return
	}
	for _, c := range [...]struct{ name, content string }{
		{s.VMDatastore, "images"}, {s.ImageDatastore, "import"}, {s.BootstrapDatastore, "iso"},
	} {
		for _, node := range s.Nodes {
			st := slices.IndexFunc(inv.Storages, func(x pve.Storage) bool { return x.Storage == c.name && x.Node == node })
			switch {
			case st < 0:
				f.Errors = append(f.Errors, fmt.Sprintf("Storage %s is not enabled on node %s. Enable it there under Datacenter, Storage, Edit, Nodes, or choose another storage.", c.name, node))
			case !inv.Storages[st].Active:
				f.Errors = append(f.Errors, fmt.Sprintf("Storage %s is not active on node %s. Check its connection there, or choose another storage.", c.name, node))
			case !slices.Contains(inv.Storages[st].Content, c.content):
				f.Errors = append(f.Errors, fmt.Sprintf("Storage %s on node %s lacks the %s content type. Add it under Datacenter, Storage, Edit, Content.", c.name, node, c.content))
			}
		}
	}
}

// checkDiskSpace checks each node's VM disks fit in the VM storage. A shared storage holds the
// disks of every node at once, so it is checked against all of them.
func (s *Settings) checkDiskSpace(inv *Inventory, f *Findings) {
	vms := s.vmsOn()
	for _, node := range s.Nodes {
		i := slices.IndexFunc(inv.Storages, func(x pve.Storage) bool { return x.Storage == s.VMDatastore && x.Node == node })
		if i < 0 || !inv.Storages[i].Active {
			continue
		}
		st := inv.Storages[i]
		count := vms[node]
		where := " on " + node
		if st.Shared {
			count = len(s.NodeAddresses)
			where = ", shared by every node,"
		}
		if need := int64(count) * vmDiskGiB * gib; st.Avail < need {
			diskShortage(&st, fmt.Sprintf("has %s free%s for %s of disks.", size(st.Avail), where, size(need)), f)
		}
		if st.Shared {
			return
		}
	}
}

// diskShortage says that the disks do not fit in the free space. Where the storage takes space as
// the guests write, Proxmox VE allows that, so it is a warning. Where it takes it at once, or where
// Proxmox VE did not say, the run would fail or may.
func diskShortage(st *pve.Storage, has string, f *Findings) {
	switch thin, known := st.AllocatesOnWrite(); {
	case !known:
		f.Warnings = append(f.Warnings, fmt.Sprintf("Storage %s %s Proxmox VE did not say whether it reserves the space.", st.Storage, has))
	case thin:
		f.Warnings = append(f.Warnings, fmt.Sprintf("Storage %s (%s) %s Writes fail once it fills.", st.Storage, st.Type, has))
	default:
		f.Errors = append(f.Errors, fmt.Sprintf("Storage %s %s Free space there, or choose another storage.", st.Storage, has))
	}
}

// checkBridge holds every bridge to the zone Proxmox VE has it in, since the module grants SDN.Use
// in the zone the settings name, and checks that each management node carries the management one.
func (s *Settings) checkBridge(inv *Inventory, f *Findings) {
	zone, name := pve.SplitBridge(s.Bridge)
	for i, ref := range append([]string{s.Bridge}, s.WorkloadBridges...) {
		z, n := pve.SplitBridge(ref)
		network, known := inv.Networks[n]
		if !known {
			continue
		}
		switch network.Zone {
		case z:
		case pve.LocalZone:
			// Proxmox VE answers alike for a VNet that does not exist and one the user may not see.
			f.Errors = append(f.Errors, fmt.Sprintf("Bridge %s: Proxmox VE shows you no VNet %s. A node's own bridge is written as %s.", ref, n, n))
		default:
			f.Errors = append(f.Errors, fmt.Sprintf("Bridge %s is in zone %s. Write it as %s/%s.", ref, network.Zone, network.Zone, n))
		}
		if i == 0 && s.VLAN != nil && *s.VLAN > 0 && network.TagsTraffic() {
			f.Errors = append(f.Errors, fmt.Sprintf("VNet %s is in a VLAN zone and tags already. Leave the VLAN tag empty.", n))
		}
	}
	for _, node := range s.Nodes {
		bridges, known := inv.Bridges[node]
		if !known || slices.ContainsFunc(bridges, func(b pve.Bridge) bool { return b.Bridge == name }) {
			continue
		}
		if zone == pve.LocalZone {
			f.Errors = append(f.Errors, fmt.Sprintf("Node %s has no bridge %s. Create it under %s, System, Network, or choose another bridge.", node, name, node))
			continue
		}
		// A VNet the user may not read is missing from every node, and Proxmox VE judges it at the clone.
		if _, readable := inv.Networks[name]; readable {
			f.Errors = append(f.Errors, fmt.Sprintf("Node %s does not carry VNet %s. Add the node to zone %s, or choose another bridge.", node, name, zone))
		}
	}
}

func (s *Settings) checkMemory(inv *Inventory, f *Findings) {
	for node, count := range s.vmsOn() {
		i := slices.IndexFunc(inv.Nodes, func(n pve.Node) bool { return n.Node == node })
		if i < 0 || inv.Nodes[i].MaxMemory == 0 {
			continue
		}
		free, need := inv.Nodes[i].MaxMemory-inv.Nodes[i].Memory, int64(count)*vmMemoryMiB*mib
		if free < need {
			f.Errors = append(f.Errors, fmt.Sprintf("Node %s has %s memory free, and its %s need %s. Free memory there, or add nodes.", node, size(free), plural(count, "management VM"), size(need)))
		}
	}
}

func (s *Settings) checkAddressesFree(inv *Inventory, f *Findings) {
	addrs := make([]string, 1, 1+len(s.NodeAddresses))
	addrs[0] = s.APIAddress
	for _, a := range s.NodeAddresses {
		ip, _, _ := strings.Cut(a, "/")
		addrs = append(addrs, ip)
	}
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && inv.Answering[ip] {
			f.Errors = append(f.Errors, fmt.Sprintf("Address %s answers to ping or a connection already. Choose an address no host uses.", a))
		}
	}
}

// checkVMIDs checks the range the management VMs' IDs are drawn from still holds a free ID for each
// of them. A running management cluster keeps the IDs it has, so this is left out for it.
func (s *Settings) checkVMIDs(inv *Inventory, f *Findings) {
	if inv.UsedVMIDs == nil {
		return
	}
	need := len(s.NodeAddresses)
	r := ChooseVMIDRange(inv.NextID, need)
	if free := r.Free(inv.UsedVMIDs); free < need {
		f.Errors = append(f.Errors, fmt.Sprintf("VM IDs %d to %d, the range Proxmox VE offers new VMs from, have %s free, and the management cluster needs %d. Widen next-id under Datacenter, Options.", r.Start, r.End-1, plural(max(free, 0), "ID"), need))
	}
}

func (s *Settings) checkNamesFree(inv *Inventory, f *Findings) {
	made := s.moduleNames()
	for _, c := range [...]struct {
		kind, where string
		names       []string
		have        map[string]bool
	}{
		{"Pool", "Datacenter, Permissions, Pools", made.pools, inv.Pools},
		{"Role", "Datacenter, Permissions, Roles", made.roles, inv.Roles},
		{"User", "Datacenter, Permissions, Users", made.users, inv.Users},
		{"Group", "Datacenter, Permissions, Groups", made.groups, inv.Groups},
	} {
		for _, n := range c.names {
			if c.have[n] {
				f.Errors = append(f.Errors, fmt.Sprintf("%s %s exists in Proxmox VE already, and the setup would fail making it. Remove it under %s if it is a leftover.", c.kind, n, c.where))
			}
		}
	}
}

// warnPlacement warns of what works but weakens the cluster: VMs sharing a node, and an API address
// that is one node's, which every cluster then depends on.
func (s *Settings) warnPlacement(inv *Inventory, f *Findings) {
	if n := len(s.NodeAddresses); n > 1 && len(s.Nodes) < n {
		f.Warnings = append(f.Warnings, fmt.Sprintf("%s share %s. One node failing then takes more than one etcd member down. Choose %s to survive a node failure.", plural(n, "management VM"), plural(len(s.Nodes), "Proxmox VE node"), plural(n, "node")))
	}
	u, err := url.Parse(inv.ProxmoxURL)
	if err != nil || u.Hostname() == "" {
		return
	}
	host := u.Hostname()
	ip, _ := netip.ParseAddr(host)
	for node, addrs := range inv.NodeAddresses {
		if host == node || (ip.IsValid() && slices.Contains(addrs, ip)) {
			f.Warnings = append(f.Warnings, fmt.Sprintf("The Proxmox VE address %s is node %s alone. Every cluster stops managing VMs while that node is down. Use a name or address that reaches any node.", host, node))
			return
		}
	}
}

// warnRanges warns when the pod or service range overlaps a network a node's bridge sits on, or a
// node's own address, which pods could then not reach. It also reports the ranges the run will use.
func (s *Settings) warnRanges(inv *Inventory, f *Findings) {
	ranges, err := s.ranges()
	if err != nil {
		return
	}
	f.PodRanges, f.ServiceRanges = ranges.pods, ranges.services
	places := networkPlaces(inv)
	for _, r := range [...]struct {
		label  string
		ranges []string
	}{{"pod", ranges.pods}, {"service", ranges.services}} {
		for _, c := range r.ranges {
			prefix, err := netip.ParsePrefix(c)
			if err != nil {
				continue
			}
			seen := map[string]bool{}
			for _, p := range places {
				if prefix.Overlaps(p.prefix) && !seen[p.what] {
					seen[p.what] = true
					f.Warnings = append(f.Warnings, fmt.Sprintf("The %s range %s overlaps %s. Pods could not reach hosts there. Choose another range under Advanced.", r.label, c, p.what))
				}
			}
		}
	}
}

// place is somewhere on the network that pods could not reach from inside an overlapping range.
type place struct {
	prefix netip.Prefix
	what   string
}

// networkPlaces are the networks, gateways and addresses of the nodes' bridges and the nodes
// themselves.
func networkPlaces(inv *Inventory) []place {
	var places []place
	for _, node := range slices.Sorted(maps.Keys(inv.Bridges)) {
		for i := range inv.Bridges[node] {
			b := &inv.Bridges[node][i]
			if p, err := netip.ParsePrefix(b.CIDR); err == nil {
				places = append(places, place{p.Masked(), fmt.Sprintf("the network %s on bridge %s of node %s", p.Masked(), b.Bridge, node)})
			}
			if g, err := netip.ParseAddr(b.Gateway); err == nil {
				places = append(places, place{netip.PrefixFrom(g, g.BitLen()), fmt.Sprintf("the gateway %s of bridge %s on node %s", g, b.Bridge, node)})
			}
		}
	}
	for _, node := range slices.Sorted(maps.Keys(inv.NodeAddresses)) {
		for _, a := range inv.NodeAddresses[node] {
			places = append(places, place{netip.PrefixFrom(a, a.BitLen()), fmt.Sprintf("the address %s of node %s", a, node)})
		}
	}
	return places
}

// plural is "1 node" or "3 nodes", with the noun in its singular form.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// size is a byte count in GiB with one decimal, or MiB below one GiB.
func size(b int64) string {
	if b < gib {
		return fmt.Sprintf("%d MiB", b/mib)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
}
