package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// The wait between asks after a task starts at firstTaskPoll and grows by taskPollGrowth each
	// time, up to lastTaskPoll.
	firstTaskPoll  = 100 * time.Millisecond
	lastTaskPoll   = 2 * time.Second
	taskPollGrowth = 2
)

// Realm is a Proxmox VE authentication realm users sign in through.
type Realm struct {
	Realm   string `json:"realm"`
	Type    string `json:"type"`
	Comment string `json:"comment,omitempty"`
	Default bool   `json:"default,omitempty"`
}

// Storage is a storage on one node, as the user sees it.
type Storage struct {
	Storage string   `json:"storage"`
	Node    string   `json:"node"`
	Shared  bool     `json:"shared"`
	Content []string `json:"content"`
	// Active tells whether Proxmox VE reports the storage as available on the node, which an offline
	// NFS or CIFS share is not.
	Active bool `json:"active"`
	// Type is the storage plugin, such as lvm, lvmthin, zfspool, rbd, dir or nfs.
	Type string `json:"type,omitempty"`
	// Sparse tells whether a ZFS pool allocates a disk's space on write. Nil where Proxmox VE did not
	// say, which the user's privileges can cause.
	Sparse *bool `json:"sparse,omitempty"`
	// Preallocation is how a storage that keeps files lays out a new disk: off, metadata, falloc or
	// full. Empty is Proxmox VE's default, off.
	Preallocation string `json:"preallocation,omitempty"`
	// Avail and Total are bytes. They are zero when the storage is not active on the node.
	Avail int64 `json:"avail"`
	Total int64 `json:"total"`
}

// Node is a Proxmox VE node as the user sees it.
type Node struct {
	Node   string `json:"node"`
	Online bool   `json:"online"`
	// Memory is what the node uses and MaxMemory what it has, in bytes, 0 where Proxmox VE does not
	// say, as for a node that is down.
	Memory    int64 `json:"memory,omitempty"`
	MaxMemory int64 `json:"maxMemory,omitempty"`
}

// Bridge is a Linux or Open vSwitch bridge on a node.
type Bridge struct {
	Bridge    string `json:"bridge"`
	Node      string `json:"node"`
	VLANAware bool   `json:"vlanAware"`
	Comment   string `json:"comment,omitempty"`
	// CIDR and Gateway are the node's own IPv4 address on the bridge and its gateway, where it has them.
	CIDR    string `json:"cidr,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// Zone is the SDN zone of a VNet. A node's own bridge has none and sits in LocalZone.
	Zone string `json:"zone,omitempty"`
	// ZoneType is the type of the VNet's zone: simple, vlan, qinq, vxlan or evpn. Empty where the
	// user may not read the zone.
	ZoneType string `json:"zoneType,omitempty"`
	// Alias is the VNet's display name, which Proxmox VE's interface shows beside its ID.
	Alias string `json:"alias,omitempty"`
}

// LocalZone is the SDN zone of the bridges in a node's network configuration.
const LocalZone = "localnetwork"

// Ref is how a bridge is written in settings and in the management-cluster module: a VNet as
// zone/vnet, a node's bridge as its name.
func (b *Bridge) Ref() string {
	if b.Zone == "" {
		return b.Bridge
	}
	return b.Zone + "/" + b.Bridge
}

// InZone is the zone Proxmox VE checks SDN.Use in for the bridge.
func (b *Bridge) InZone() string {
	if b.Zone == "" {
		return LocalZone
	}
	return b.Zone
}

// SplitBridge splits a bridge reference into its zone and the name a VM's network device takes.
func SplitBridge(ref string) (string, string) {
	if zone, name, ok := strings.Cut(ref, "/"); ok {
		return zone, name
	}
	return LocalZone, ref
}

// Network is where Proxmox VE checks the use of a bridge or VNet, and whether the VNet tags its
// traffic already.
type Network struct {
	Zone string
	// ZoneType is empty for a node's bridge and where the user may not read the zone.
	ZoneType  string
	VLANAware bool
}

// TagsTraffic tells whether the network puts every frame in a VLAN of its own, so that a VLAN tag on
// a VM's device would add a second one. A VLAN-aware VNet leaves the tag to the device.
func (n Network) TagsTraffic() bool { return n.ZoneType == "vlan" && !n.VLANAware }

// flag is a Proxmox VE boolean, which its API writes as 1 or true.
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	*f = string(b) == "1" || string(b) == "true"
	return nil
}

// HARule is an enabled node-affinity rule of Proxmox VE's HA: the nodes its resources keep to, or
// for a negative rule keep off.
type HARule struct {
	Rule    string `json:"rule"`
	Comment string `json:"comment,omitempty"`
	// Nodes are the rule's nodes, without the priorities Proxmox VE ranks them by.
	Nodes    []string `json:"nodes"`
	Negative bool     `json:"negative,omitempty"`
	// Strict rules keep their resources to the nodes even when none of them is up.
	Strict bool `json:"strict,omitempty"`
}

// Member is a guest in a pool.
type Member struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	Type string `json:"type"`
	Name string `json:"name"`
	// Template is 1 for a VM template.
	Template int `json:"template"`
	// Tags are separated by semicolons, as Proxmox VE stores them. Templates reads them, PoolMembers
	// cannot.
	Tags string `json:"tags"`
	// Status is running or stopped. CPU is the share of its cores in use, from 0 to 1. Memory is in
	// bytes, uptime in seconds. Proxmox VE lists them with a pool's members, from its nodes' last
	// broadcast.
	Status string  `json:"status"`
	CPU    float64 `json:"cpu"`
	Cores  float64 `json:"maxcpu"`
	Memory int64   `json:"mem"`
	MaxMem int64   `json:"maxmem"`
	Uptime int64   `json:"uptime"`
}

// LoadPoint is a VM's average CPU load over one step of Proxmox VE's round-robin data, the share of
// its cores in use from 0 to 1. It is nil where the VM did not run.
type LoadPoint struct {
	Time int64    `json:"time"`
	CPU  *float64 `json:"cpu"`
}

// VMLoad reads a VM's CPU load over the last hour, a point a minute, as Proxmox VE keeps it.
func (a *Accounts) VMLoad(ctx context.Context, t *Ticket, node string, vmid int) ([]LoadPoint, error) {
	var points []LoadPoint
	path := fmt.Sprintf("/nodes/%s/qemu/%d/rrddata?timeframe=hour&cf=AVERAGE", url.PathEscape(node), vmid)
	if err := a.get(ctx, t, path, &points); err != nil {
		return nil, err
	}
	return points, nil
}

// Realms lists the realms, as Proxmox VE shows them to anyone before sign-in.
func (a *Accounts) Realms(ctx context.Context) ([]Realm, error) {
	req, err := a.newRequest(ctx, http.MethodGet, "/access/domains", http.NoBody)
	if err != nil {
		return nil, err
	}
	var res []struct {
		Realm   string `json:"realm"`
		Type    string `json:"type"`
		Comment string `json:"comment"`
		Default int    `json:"default"`
	}
	if err := a.do(req, &res); err != nil {
		return nil, err
	}
	realms := make([]Realm, 0, len(res))
	for _, r := range res {
		realms = append(realms, Realm{Realm: r.Realm, Type: r.Type, Comment: r.Comment, Default: r.Default != 0})
	}
	// Proxmox VE lists them in its hash order, which changes from one call to the next.
	slices.SortFunc(realms, func(x, y Realm) int { return strings.Compare(x.Realm, y.Realm) })
	return realms, nil
}

// isZFS is a storage plugin that makes a ZFS volume per disk, which holds its space back unless the
// storage is sparse.
func isZFS(plugin string) bool { return plugin == "zfspool" || plugin == "zfs" }

// Disk formats a VM disk is made in on a storage that keeps files.
const (
	FormatRaw   = "raw"
	FormatQCOW2 = "qcow2"
)

// AllocatesOnWrite tells whether a disk takes its space as the guest writes, which Proxmox VE lets
// exceed the free space. LVM-thin and Ceph RBD always do. A ZFS volume does when the storage is
// sparse, and otherwise reserves its whole size. Thick LVM reserves it too. On storage that keeps
// files, qemu-img makes a sparse file, unless the storage's preallocation is falloc or full.
// The second result is false where Proxmox VE did not say which type the storage is, or whether a
// ZFS storage is sparse, and the first is then meaningless.
func (s *Storage) AllocatesOnWrite() (bool, bool) {
	switch {
	case s.Type == "lvmthin" || s.Type == "rbd":
		return true, true
	case isZFS(s.Type):
		if s.Sparse == nil {
			return false, false
		}
		return *s.Sparse, true
	case s.Type == "lvm":
		return false, true
	case s.Type == "":
		return false, false
	default:
		return s.Preallocation != "falloc" && s.Preallocation != "full", true
	}
}

// Storages lists the storages on every node the user may see, with the content types each takes.
func (a *Accounts) Storages(ctx context.Context, t *Ticket) ([]Storage, error) {
	var res []struct {
		Storage string `json:"storage"`
		Node    string `json:"node"`
		Shared  int    `json:"shared"`
		Content string `json:"content"`
		Type    string `json:"plugintype"`
		Disk    int64  `json:"disk"`
		MaxDisk int64  `json:"maxdisk"`
		Status  string `json:"status"`
	}
	if err := a.get(ctx, t, "/cluster/resources?type=storage", &res); err != nil {
		return nil, err
	}
	configs := a.storageConfigs(ctx, t)
	var storages []Storage
	for _, r := range res {
		content := strings.Split(r.Content, ",")
		s := Storage{Storage: r.Storage, Node: r.Node, Shared: r.Shared != 0, Content: content, Type: r.Type}
		if c, ok := configs[r.Storage]; ok {
			s.Preallocation = c.preallocation
			if isZFS(r.Type) {
				s.Sparse = &c.sparse
			}
		}
		if r.Status == "available" {
			s.Active = true
			s.Avail, s.Total = r.MaxDisk-r.Disk, r.MaxDisk
		}
		storages = append(storages, s)
	}
	return storages, nil
}

// storageConfig is what the storage configuration says of how a storage allocates, which the
// cluster's resources leave out.
type storageConfig struct {
	sparse        bool
	preallocation string
}

// storageConfigs reads the storage configuration. A read that fails, as for a user without the right
// to it, leaves every storage's answer unknown.
func (a *Accounts) storageConfigs(ctx context.Context, t *Ticket) map[string]storageConfig {
	var res []struct {
		Storage       string `json:"storage"`
		Sparse        any    `json:"sparse"`
		Preallocation string `json:"preallocation"`
	}
	if err := a.get(ctx, t, "/storage", &res); err != nil {
		return nil
	}
	out := make(map[string]storageConfig, len(res))
	for _, r := range res {
		c := storageConfig{preallocation: r.Preallocation}
		// Proxmox VE leaves the key out of a pool that never set it, which means thick.
		switch v := r.Sparse.(type) {
		case float64:
			c.sparse = v != 0
		case bool:
			c.sparse = v
		case string:
			c.sparse = v == "1" || v == "true"
		}
		out[r.Storage] = c
	}
	return out
}

// Nodes lists the nodes the user may see.
func (a *Accounts) Nodes(ctx context.Context, t *Ticket) ([]Node, error) {
	var res []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
		Mem    int64  `json:"mem"`
		MaxMem int64  `json:"maxmem"`
	}
	if err := a.get(ctx, t, "/cluster/resources?type=node", &res); err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(res))
	for _, r := range res {
		nodes = append(nodes, Node{Node: r.Node, Online: r.Status == "online", Memory: r.Mem, MaxMemory: r.MaxMem})
	}
	return nodes, nil
}

// Bridges lists a node's bridges. Proxmox VE shows them to users with Sys.Audit on the node.
func (a *Accounts) Bridges(ctx context.Context, t *Ticket, node string) ([]Bridge, error) {
	var res []struct {
		Iface     string `json:"iface"`
		VLANAware int    `json:"bridge_vlan_aware"`
		Comments  string `json:"comments"`
		CIDR      string `json:"cidr"`
		Gateway   string `json:"gateway"`
	}
	if err := a.get(ctx, t, "/nodes/"+url.PathEscape(node)+"/network?type=any_bridge", &res); err != nil {
		return nil, err
	}
	bridges := make([]Bridge, 0, len(res))
	for _, r := range res {
		bridges = append(bridges, Bridge{
			Bridge: r.Iface, Node: node, VLANAware: r.VLANAware != 0, Comment: strings.TrimSpace(r.Comments),
			CIDR: r.CIDR, Gateway: r.Gateway,
		})
	}
	return append(bridges, a.vnets(ctx, t, node)...), nil
}

// vnets lists the VNets a node carries: those of zones that name it or no node. The user sees the
// VNets they hold SDN.Audit on. Where SDN is absent or unreadable the node has its bridges only.
func (a *Accounts) vnets(ctx context.Context, t *Ticket, node string) []Bridge {
	var vnets []struct {
		VNet      string `json:"vnet"`
		Zone      string `json:"zone"`
		Alias     string `json:"alias"`
		VLANAware flag   `json:"vlanaware"`
	}
	if a.get(ctx, t, "/cluster/sdn/vnets", &vnets) != nil {
		return nil
	}
	var zones []struct {
		Zone  string `json:"zone"`
		Type  string `json:"type"`
		Nodes string `json:"nodes"`
	}
	// Without the zones every VNet is offered, on the strength of its own zone name.
	_ = a.get(ctx, t, "/cluster/sdn/zones", &zones)
	type zoneInfo struct {
		kind  string
		nodes []string
	}
	known := map[string]zoneInfo{}
	for _, z := range zones {
		info := zoneInfo{kind: z.Type}
		for n := range strings.SplitSeq(z.Nodes, ",") {
			if n = strings.TrimSpace(n); n != "" {
				info.nodes = append(info.nodes, n)
			}
		}
		known[z.Zone] = info
	}
	var bridges []Bridge
	for _, v := range vnets {
		z := known[v.Zone]
		if v.VNet == "" || v.Zone == "" || len(z.nodes) > 0 && !slices.Contains(z.nodes, node) {
			continue
		}
		bridges = append(bridges, Bridge{
			Bridge: v.VNet, Node: node, VLANAware: bool(v.VLANAware), Zone: v.Zone, ZoneType: z.kind,
			Alias: strings.TrimSpace(v.Alias),
		})
	}
	slices.SortFunc(bridges, func(x, y Bridge) int { return strings.Compare(x.Bridge, y.Bridge) })
	return bridges
}

// NodeDNS is how a node resolves names: its DNS servers in order, and its search domain, which is
// empty when the node has none.
type NodeDNS struct {
	Servers []string `json:"servers"`
	Search  string   `json:"search"`
}

// DNS reads a node's resolver settings. Proxmox VE shows them to users with Sys.Audit on the node.
func (a *Accounts) DNS(ctx context.Context, t *Ticket, node string) (NodeDNS, error) {
	var res struct {
		Search string `json:"search"`
		DNS1   string `json:"dns1"`
		DNS2   string `json:"dns2"`
		DNS3   string `json:"dns3"`
	}
	if err := a.get(ctx, t, "/nodes/"+url.PathEscape(node)+"/dns", &res); err != nil {
		return NodeDNS{}, err
	}
	dns := NodeDNS{Servers: []string{}, Search: strings.TrimSpace(res.Search)}
	for _, s := range []string{res.DNS1, res.DNS2, res.DNS3} {
		if s = strings.TrimSpace(s); s != "" {
			dns.Servers = append(dns.Servers, s)
		}
	}
	return dns, nil
}

// Names are the IDs of the pools, roles, users and groups the user may see. A listing the user may
// not read is nil.
type Names struct {
	Pools, Roles, Users, Groups map[string]bool
}

// AccessNames lists the IDs of the pools, roles, users and groups, for telling whether what a setup
// would create exists already.
func (a *Accounts) AccessNames(ctx context.Context, t *Ticket) (Names, error) {
	var names Names
	for _, l := range [...]struct {
		path, key string
		into      *map[string]bool
	}{
		{"/pools", "poolid", &names.Pools},
		{"/access/roles", "roleid", &names.Roles},
		{"/access/users", "userid", &names.Users},
		{"/access/groups", "groupid", &names.Groups},
	} {
		var res []map[string]any
		err := a.get(ctx, t, l.path, &res)
		if errors.Is(err, ErrForbidden) {
			continue
		}
		if err != nil {
			return Names{}, err
		}
		ids := make(map[string]bool, len(res))
		for _, r := range res {
			if id, ok := r[l.key].(string); ok {
				ids[id] = true
			}
		}
		*l.into = ids
	}
	return names, nil
}

// NodeAddresses lists the addresses Proxmox VE's cluster reports for its nodes, by node. A node on
// its own reports none.
func (a *Accounts) NodeAddresses(ctx context.Context, t *Ticket) (map[string]string, error) {
	var res []struct {
		Type string `json:"type"`
		Name string `json:"name"`
		IP   string `json:"ip"`
	}
	if err := a.get(ctx, t, "/cluster/status", &res); err != nil {
		return nil, err
	}
	addresses := map[string]string{}
	for _, r := range res {
		if r.Type == "node" && r.IP != "" {
			addresses[r.Name] = r.IP
		}
	}
	return addresses, nil
}

// Allowed are those of the nodes given that the rule keeps its resources to: its own, or for a
// negative rule every other one.
func (r *HARule) Allowed(nodes []string) []string {
	var allowed []string
	for _, n := range nodes {
		if slices.Contains(r.Nodes, n) != r.Negative {
			allowed = append(allowed, n)
		}
	}
	return allowed
}

// NodeAffinityPath lists Proxmox VE's HA node-affinity rules, to anyone with Sys.Audit on /.
const NodeAffinityPath = "/cluster/ha/rules?type=node-affinity"

// HARuleEntry is a rule as NodeAffinityPath lists it. Proxmox VE leaves out a flag that is off,
// and writes one that is on as 1.
type HARuleEntry struct {
	Rule     string `json:"rule"`
	Comment  string `json:"comment"`
	Nodes    string `json:"nodes"`
	Affinity string `json:"affinity"`
	Strict   int    `json:"strict"`
	Disable  int    `json:"disable"`
}

// NodeAffinityRules lists the enabled HA node-affinity rules. Proxmox VE shows them to users with
// Sys.Audit on /.
func (a *Accounts) NodeAffinityRules(ctx context.Context, t *Ticket) ([]HARule, error) {
	var res []HARuleEntry
	if err := a.get(ctx, t, NodeAffinityPath, &res); err != nil {
		return nil, err
	}
	return EnabledRules(res), nil
}

// EnabledRules are the rules of a listing that are not disabled.
func EnabledRules(res []HARuleEntry) []HARule {
	rules := make([]HARule, 0, len(res))
	for _, r := range res {
		if r.Disable != 0 {
			continue
		}
		rule := HARule{Rule: r.Rule, Comment: r.Comment, Nodes: []string{}, Negative: r.Affinity == "negative", Strict: r.Strict != 0}
		for entry := range strings.SplitSeq(r.Nodes, ",") {
			// An entry is node or node:priority.
			if node, _, _ := strings.Cut(entry, ":"); node != "" {
				rule.Nodes = append(rule.Nodes, node)
			}
		}
		rules = append(rules, rule)
	}
	return rules
}

// Network looks up the bridge a VM connects to: a VNet's own zone, or LocalZone for a plain bridge,
// as Proxmox VE decides where SDN.Use is checked. A VNet the user may not see reads as a plain
// bridge on releases that answer both alike, and Proxmox VE still checks its zone at the clone.
func (a *Accounts) Network(ctx context.Context, t *Ticket, bridge string) (Network, error) {
	var vnet struct {
		Zone      string `json:"zone"`
		VLANAware flag   `json:"vlanaware"`
	}
	err := a.get(ctx, t, "/cluster/sdn/vnets/"+url.PathEscape(bridge), &vnet)
	if notFound(err) || unknownVNet(err) {
		return Network{Zone: LocalZone}, nil
	}
	if err != nil {
		return Network{}, err
	}
	if vnet.Zone == "" {
		return Network{}, fmt.Errorf("proxmox VE names no zone for VNet %s", bridge)
	}
	n := Network{Zone: vnet.Zone, VLANAware: bool(vnet.VLANAware)}
	var zone struct {
		Type string `json:"type"`
	}
	// Proxmox VE may refuse the zone to a user who may use the VNet. Without its type nothing is
	// known of the tagging, so nothing is refused for it.
	if a.get(ctx, t, "/cluster/sdn/zones/"+url.PathEscape(vnet.Zone), &zone) == nil {
		n.ZoneType = zone.Type
	}
	return n, nil
}

// BackupCovers tells whether an enabled backup job of the Proxmox VE cluster includes the guest,
// by its ID, by all guests, or by its pool. Listing the jobs needs Sys.Audit on /.
func (a *Accounts) BackupCovers(ctx context.Context, t *Ticket, vmid int) (bool, error) {
	var guests []struct {
		VMID int    `json:"vmid"`
		Node string `json:"node"`
		Pool string `json:"pool"`
	}
	if err := a.get(ctx, t, "/cluster/resources?type=vm", &guests); err != nil {
		return false, err
	}
	node, pool := "", ""
	for _, g := range guests {
		if g.VMID == vmid {
			node, pool = g.Node, g.Pool
		}
	}
	var jobs []struct {
		Enabled *int   `json:"enabled"`
		Node    string `json:"node"`
		All     int    `json:"all"`
		VMID    string `json:"vmid"`
		Exclude string `json:"exclude"`
		Pool    string `json:"pool"`
	}
	if err := a.get(ctx, t, "/cluster/backup", &jobs); err != nil {
		return false, err
	}
	id := strconv.Itoa(vmid)
	for _, j := range jobs {
		// A job limited to one node backs up only the guests on it.
		if j.Enabled != nil && *j.Enabled == 0 || j.Node != "" && j.Node != node {
			continue
		}
		switch {
		case j.All != 0 && !slices.Contains(strings.Split(j.Exclude, ","), id):
			return true, nil
		case slices.Contains(strings.Split(j.VMID, ","), id):
			return true, nil
		case j.Pool != "" && j.Pool == pool:
			return true, nil
		}
	}
	return false, nil
}

// Templates lists the VM templates in a pool, with their tags. The cluster's resource view reads
// pools and tags afresh, but a VM's name and whether it is a template from the status its node
// broadcast last, which can be seconds old and can still show a removed VM's under a reused VM ID.
// Each VM's configuration says it for certain, before anything is done to the VM by its ID.
func (a *Accounts) Templates(ctx context.Context, t *Ticket, pool string) ([]Member, error) {
	var guests []struct {
		Member

		Pool string `json:"pool"`
	}
	// A pool's own member list leaves the tags out.
	if err := a.get(ctx, t, "/cluster/resources?type=vm", &guests); err != nil {
		return nil, err
	}
	var list []Member
	for i := range guests {
		g := &guests[i]
		if g.Type != "qemu" || g.Pool != pool {
			continue
		}
		var config struct {
			Name     string `json:"name"`
			Template int    `json:"template"`
		}
		err := a.get(ctx, t, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(g.Node), g.VMID), &config)
		if notFound(err) || err == nil && config.Template != 1 {
			continue
		}
		if err != nil {
			return nil, err
		}
		g.Name, g.Template = config.Name, config.Template
		list = append(list, g.Member)
	}
	return list, nil
}

// PoolMembers lists a pool's VMs, without their tags.
func (a *Accounts) PoolMembers(ctx context.Context, t *Ticket, pool string) ([]Member, error) {
	var pools []struct {
		Members []Member `json:"members"`
	}
	// /pools/{poolid} cannot name a nested pool such as a cluster's kubehenge/clusters.web.
	err := a.get(ctx, t, "/pools?"+url.Values{"poolid": {pool}}.Encode(), &pools)
	if notFound(err) || err == nil && len(pools) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vms []Member
	for i := range pools[0].Members {
		if m := &pools[0].Members[i]; m.Type == "qemu" {
			vms = append(vms, *m)
		}
	}
	return vms, nil
}

// DeleteVM destroys a VM with its disks and waits for Proxmox VE to finish.
func (a *Accounts) DeleteVM(ctx context.Context, t *Ticket, node string, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/qemu/%d?purge=1&destroy-unreferenced-disks=1", url.PathEscape(node), vmid)
	req, err := a.newRequest(ctx, http.MethodDelete, path, http.NoBody)
	if err != nil {
		return err
	}
	t.authorise(req)
	var upid string
	if err := a.do(req, &upid); err != nil {
		return err
	}
	return a.waitTask(ctx, t, node, upid)
}

// RebootVM shuts a VM down and starts it again, as Proxmox VE's Reboot does, without waiting: the
// shutdown waits on the guest. Proxmox VE lets VM.PowerMgmt do it.
func (a *Accounts) RebootVM(ctx context.Context, t *Ticket, node string, vmid int) error {
	return a.send(ctx, t, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/reboot", url.PathEscape(node), vmid), nil, nil)
}

// ReleaseHA removes the HA rules and resources whose comment starts with prefix, the rules first,
// so that none is left naming a resource that is gone. Proxmox VE lets only Sys.Console on / do it.
func (a *Accounts) ReleaseHA(ctx context.Context, t *Ticket, prefix string) error {
	var rules []struct {
		Rule    string `json:"rule"`
		Comment string `json:"comment"`
	}
	if err := a.get(ctx, t, "/cluster/ha/rules", &rules); err != nil {
		return err
	}
	for _, r := range rules {
		if strings.HasPrefix(r.Comment, prefix) {
			if err := a.remove(ctx, t, "/cluster/ha/rules/"+url.PathEscape(r.Rule)); err != nil {
				return err
			}
		}
	}
	var resources []struct {
		SID     string `json:"sid"`
		Comment string `json:"comment"`
	}
	if err := a.get(ctx, t, "/cluster/ha/resources", &resources); err != nil {
		return err
	}
	for _, r := range resources {
		if strings.HasPrefix(r.Comment, prefix) {
			if err := a.remove(ctx, t, "/cluster/ha/resources/"+url.PathEscape(r.SID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// remove sends a DELETE that Proxmox VE answers at once, without a task.
func (a *Accounts) remove(ctx context.Context, t *Ticket, path string) error {
	req, err := a.newRequest(ctx, http.MethodDelete, path, http.NoBody)
	if err != nil {
		return err
	}
	t.authorise(req)
	var ignored any
	return a.do(req, &ignored)
}

// waitTask asks soon at first, as most tasks end within a fraction of a second, and less often the
// longer one runs.
func (a *Accounts) waitTask(ctx context.Context, t *Ticket, node, upid string) error {
	for interval := firstTaskPoll; ; interval = min(interval*taskPollGrowth, lastTaskPoll) {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := a.get(ctx, t, fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid)), &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				get := func(ctx context.Context, path string, out any) error { return a.get(ctx, t, path, out) }
				return TaskFailed(ctx, get, node, upid, st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for proxmox VE task %s: %w", upid, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// notFound tells whether Proxmox VE refused a request because what it names does not exist, which
// it reports only in its status line.
func notFound(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && strings.Contains(apiErr.Status, "does not exist")
}

// unknownVNet tells whether Proxmox VE refused the vnet parameter as a VNet it knows none of. Newer
// releases answer so for a VNet that does not exist and for one the user may not see alike, where
// older ones said "does not exist" or refused the permission.
func unknownVNet(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && strings.HasPrefix(apiErr.Errors["vnet"], "could not find vnet")
}
