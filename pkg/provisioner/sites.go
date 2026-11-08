package provisioner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// StorageSite is where one Proxmox VE storage can be used.
type StorageSite struct {
	// Shared storages are reachable from every node they are on, the others from the node they belong to.
	Shared bool
	// Nodes are the nodes the storage is configured on, and Active those where it is available.
	Nodes, Active []string
}

// TemplateSite is where one template's disks are, and how its clones are configured.
type TemplateSite struct {
	Name     string
	Node     string
	Tags     []string
	Storages []string
	// CPU is the template's own cpu setting, which its clones keep.
	CPU string
	// Hardware is what the template's hardware tags record, zero for one made before they were.
	Hardware templates.Hardware
	Built    time.Time
}

// Sites is where the storages and the templates of a Proxmox VE cluster can be used, as one look
// found them.
type Sites struct {
	Storages  map[string]StorageSite
	Templates []TemplateSite
	// Bridges are the bridges and Open vSwitch bridges of each node whose network could be read.
	Bridges map[string][]string
	// VNets give the zone of each SDN VNet the token sees, and ZoneNodes the nodes each zone is
	// applied on, for the zones the token sees.
	VNets     map[string]string
	ZoneNodes map[string][]string
}

// SiteSource looks at the storages and templates; *Proxmox is the real one.
type SiteSource interface {
	Sites(ctx context.Context) (*Sites, error)
}

// Sites reads where the storages are available and where the templates' disks are.
func (p *Proxmox) Sites(ctx context.Context) (*Sites, error) {
	var uses []pveStorageUse
	if err := p.Client.Get(ctx, "/cluster/resources?type=storage", &uses); err != nil {
		return nil, fmt.Errorf("listing the storages: %w", err)
	}
	s := &Sites{Storages: map[string]StorageSite{}}
	for _, u := range uses {
		site := s.Storages[u.Storage]
		site.Shared = site.Shared || u.Shared != 0
		site.Nodes = append(site.Nodes, u.Node)
		if u.Status == "available" {
			site.Active = append(site.Active, u.Node)
		}
		s.Storages[u.Storage] = site
	}
	inv, err := (&templates.Builder{Client: p.Client, SiteCache: &p.siteCache}).Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the Proxmox VE templates: %w", err)
	}
	for i, r := range inv.Templates {
		tags := templates.ParseTags(r.Tags)
		at, _ := templates.BuildOf(tags)
		hw, _ := templates.HardwareOf(templates.ParseAllTags(r.Tags))
		s.Templates = append(s.Templates, TemplateSite{
			Name: r.Name, Node: r.Node, Tags: tags, Storages: inv.Sites[i].Storages, CPU: inv.Sites[i].CPU, Hardware: hw, Built: at,
		})
	}
	p.readNetworks(ctx, s)
	return s, nil
}

// readNetworks reads the bridges of every online node and the SDN zones and VNets. What the token
// cannot read stays unknown, and an unknown is never a reason to leave a node out.
func (p *Proxmox) readNetworks(ctx context.Context, s *Sites) {
	log := ctrl.LoggerFrom(ctx)
	var nodes []pveNode
	if err := p.Client.Get(ctx, "/nodes", &nodes); err != nil {
		log.Error(err, "listing the nodes for their bridges")
		return
	}
	s.Bridges = map[string][]string{}
	for _, n := range nodes {
		if n.Status != nodeOnline {
			continue
		}
		var ifaces []struct {
			Iface string `json:"iface"`
		}
		if err := p.Client.Get(ctx, "/nodes/"+n.Node+"/network?type=any_bridge", &ifaces); err != nil {
			log.Error(err, "reading a node's bridges", "node", n.Node)
			continue
		}
		bridges := make([]string, len(ifaces))
		for i, iface := range ifaces {
			bridges[i] = iface.Iface
		}
		s.Bridges[n.Node] = bridges
	}
	var vnets []struct {
		VNet string `json:"vnet"`
		Zone string `json:"zone"`
	}
	if err := p.Client.Get(ctx, "/cluster/sdn/vnets", &vnets); err != nil {
		log.V(1).Info("reading the SDN VNets", "error", err.Error())
		return
	}
	s.VNets = map[string]string{}
	for _, v := range vnets {
		s.VNets[v.VNet] = v.Zone
	}
	var zones []struct {
		SDN    string `json:"sdn"`
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := p.Client.Get(ctx, "/cluster/resources?type=sdn", &zones); err != nil {
		log.V(1).Info("reading where the SDN zones are applied", "error", err.Error())
		return
	}
	s.ZoneNodes = map[string][]string{}
	for _, z := range zones {
		applied := s.ZoneNodes[z.SDN]
		if z.Status == "ok" {
			applied = append(applied, z.Node)
		}
		s.ZoneNodes[z.SDN] = applied
	}
}

// Hosts are the names of the online nodes that can run the cluster's VMs: whose CPU reaches the
// level its templates run at, where its storage is available and its bridge exists, and from which
// its templates can be cloned.
func (s *Sites) Hosts(c *clusterv1.Cluster, online []Node, fallbackLevel int) []string {
	hosts := nodesFor(online, cpuNeed(s, c, fallbackLevel))
	if storage, _, err := optionalVariable[string](c, "storage"); err == nil && storage != "" {
		if site, ok := s.Storages[storage]; ok {
			hosts = slices.DeleteFunc(hosts, func(n string) bool { return !slices.Contains(site.Active, n) })
		}
	}
	if bridge := clusterBridge(c); bridge != "" {
		hosts = slices.DeleteFunc(hosts, func(n string) bool { return !s.hasBridge(bridge, n) })
	}
	for _, line := range templateLines(c) {
		if reach, _, constrained := lineReach(s, line); constrained {
			hosts = slices.DeleteFunc(hosts, func(n string) bool { return !slices.Contains(reach, n) })
		}
	}
	return hosts
}

// Refusal says why the cluster has no node among allowed to run on, for its storage or for its
// templates, or returns nil.
func (s *Sites) Refusal(c *clusterv1.Cluster, allowed []Node) error {
	if len(allowed) == 0 {
		return nil
	}
	names := make([]string, len(allowed))
	for i, n := range allowed {
		names[i] = n.Name
	}
	if err := storageRefusal(s, c, names); err != nil {
		return err
	}
	if err := bridgeRefusal(s, c, names); err != nil {
		return err
	}
	for _, line := range templateLines(c) {
		reach, found, constrained := lineReach(s, line)
		if !constrained || slices.ContainsFunc(names, func(n string) bool { return slices.Contains(reach, n) }) {
			continue
		}
		return templateRefusal(s, &found[0], names)
	}
	return nil
}

// templateLines are the template lines a cluster's machines are cloned from. A cluster whose lines
// cannot be worked out is judged by other checks, which report why.
func templateLines(c *clusterv1.Cluster) [][]string {
	if isTalos(c) {
		image, err := TalosImage(c)
		if err != nil {
			return nil
		}
		line, err := TalosLine(image)
		if err != nil {
			return nil
		}
		return [][]string{line}
	}
	lines, err := ClusterLines(c)
	if err != nil {
		return nil
	}
	return lines
}

// newest are the templates of the newest build of a line.
func newest(s *Sites, line []string) []TemplateSite {
	key := templates.LineKey(line)
	var out []TemplateSite
	for i := range s.Templates {
		t := &s.Templates[i]
		if templates.LineKey(t.Tags) != key {
			continue
		}
		switch {
		case len(out) == 0 || t.Built.After(out[0].Built):
			out = append(out[:0], *t)
		case t.Built.Equal(out[0].Built):
			out = append(out, *t)
		}
	}
	return out
}

// reach are the nodes a template can be cloned on: those every one of its storages reaches. A
// storage is reached from every node it is available on when it is shared, otherwise only from the
// template's own node. ok is false for a template with no disk on a storage.
func reach(s *Sites, t *TemplateSite) ([]string, bool) {
	var nodes []string
	for i, name := range t.Storages {
		site, known := s.Storages[name]
		var here []string
		switch {
		case known && site.Shared:
			here = site.Active
		case !known || slices.Contains(site.Active, t.Node):
			here = []string{t.Node}
		}
		if i == 0 {
			nodes = slices.Clone(here)
		} else {
			nodes = slices.DeleteFunc(nodes, func(n string) bool { return !slices.Contains(here, n) })
		}
	}
	return nodes, len(t.Storages) > 0
}

// lineReach are the nodes any template of the newest build of a line can be cloned on.
func lineReach(s *Sites, line []string) ([]string, []TemplateSite, bool) {
	var nodes []string
	found := newest(s, line)
	for i := range found {
		reached, ok := reach(s, &found[i])
		if !ok {
			return nil, found, false
		}
		for _, n := range reached {
			if !slices.Contains(nodes, n) {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, found, len(found) > 0
}

// cpuNeed is the x86-64 level the cluster's nodes need of its hosts: the one its templates' own cpu
// setting presents when they set one, else fallback, the node OS's.
func cpuNeed(s *Sites, c *clusterv1.Cluster, fallback int) int {
	if cpu, set := templateCPU(s, c); set {
		return cpu.level
	}
	return fallback
}

// templateSetting is a template's cpu setting: the x86-64 level it presents and its model.
type templateSetting struct {
	level int
	model string
}

// templateCPU is the cpu setting of the cluster's newest templates that presents the highest
// x86-64 level. It is false where none of them sets one.
func templateCPU(s *Sites, c *clusterv1.Cluster) (templateSetting, bool) {
	var best templateSetting
	set := false
	for _, line := range templateLines(c) {
		found := newest(s, line)
		for i := range found {
			if level, ok := templates.SettingLevel(found[i].CPU); ok && (!set || level > best.level) {
				best, set = templateSetting{level: level, model: templates.SettingModel(found[i].CPU)}, true
			}
		}
	}
	return best, set
}

// templateNUMA tells whether a newest template of the cluster's lines gives its clones a NUMA
// topology.
func templateNUMA(s *Sites, c *clusterv1.Cluster) bool {
	for _, line := range templateLines(c) {
		if slices.ContainsFunc(newest(s, line), func(t TemplateSite) bool { return t.Hardware.NUMA }) {
			return true
		}
	}
	return false
}

func storageRefusal(s *Sites, c *clusterv1.Cluster, names []string) error {
	storage, _, err := optionalVariable[string](c, "storage")
	if err != nil || storage == "" || len(s.Storages) == 0 {
		return err
	}
	site, ok := s.Storages[storage]
	switch {
	case !ok:
		return fmt.Errorf("storage %s does not exist or Kubehenge's token cannot see it. Choose another storage", storage)
	case slices.ContainsFunc(names, func(n string) bool { return slices.Contains(site.Active, n) }):
		return nil
	case len(site.Active) == 0:
		return fmt.Errorf("storage %s is not available on any node (it is configured on %s). Bring it online or choose another storage",
			storage, strings.Join(site.Nodes, ", "))
	}
	return fmt.Errorf("storage %s is available only on %s, not on the nodes this cluster may use (%s). Choose another storage or allow one of those nodes",
		storage, strings.Join(site.Active, ", "), strings.Join(names, ", "))
}

// clusterBridge is the bridge or VNet the cluster's VMs attach to, or "".
func clusterBridge(c *clusterv1.Cluster) string {
	network, _, err := optionalVariable[nodeNetwork](c, "network")
	if err != nil {
		return ""
	}
	return network.Bridge
}

// hasBridge tells whether a VM attached to the bridge or VNet can run on the node. Only what is
// known counts against a node: one whose network could not be read, a VNet or zone the token does
// not see, and a name found on no node at all (which cannot be told from a VNet the token does not
// see) leave the node in.
func (s *Sites) hasBridge(bridge, node string) bool {
	if zone, isVNet := s.VNets[bridge]; isVNet {
		applied, known := s.ZoneNodes[zone]
		return !known || slices.Contains(applied, node)
	}
	list, read := s.Bridges[node]
	if !read || slices.Contains(list, bridge) {
		return true
	}
	return !slices.ContainsFunc(slices.Collect(maps.Values(s.Bridges)), func(other []string) bool { return slices.Contains(other, bridge) })
}

// bridgeNodes are the nodes the bridge or VNet is known to be on, sorted.
func (s *Sites) bridgeNodes(bridge string) []string {
	var nodes []string
	if zone, isVNet := s.VNets[bridge]; isVNet {
		nodes = slices.Clone(s.ZoneNodes[zone])
	} else {
		for node, list := range s.Bridges {
			if slices.Contains(list, bridge) {
				nodes = append(nodes, node)
			}
		}
	}
	slices.Sort(nodes)
	return nodes
}

func bridgeRefusal(s *Sites, c *clusterv1.Cluster, names []string) error {
	bridge := clusterBridge(c)
	if bridge == "" || slices.ContainsFunc(names, func(n string) bool { return s.hasBridge(bridge, n) }) {
		return nil
	}
	kind, fix := "bridge", "Create it on one of them"
	if _, isVNet := s.VNets[bridge]; isVNet {
		kind, fix = "VNet", "Apply its zone to one of them"
	}
	on := "no node"
	if nodes := s.bridgeNodes(bridge); len(nodes) > 0 {
		on = strings.Join(nodes, ", ")
	}
	return fmt.Errorf("%s %s is on %s, not on the nodes this cluster may use (%s). %s or allow a node that has it",
		kind, bridge, on, strings.Join(names, ", "), fix)
}

func templateRefusal(s *Sites, t *TemplateSite, names []string) error {
	for _, name := range t.Storages {
		if site, ok := s.Storages[name]; ok && site.Shared {
			continue
		}
		return fmt.Errorf("template %s is on storage %s, which is not shared, so it clones only on node %s. "+
			"Put the templates on shared storage or allow node %s", t.Name, name, t.Node, t.Node)
	}
	return fmt.Errorf("template %s is on storage %s, which is not available on any node this cluster may use (%s). Bring the storage online on one of them or allow another node",
		t.Name, strings.Join(t.Storages, ", "), strings.Join(names, ", "))
}
