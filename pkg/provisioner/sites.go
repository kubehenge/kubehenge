package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

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
	CPU   string
	Built time.Time
}

// Sites is where the storages and the templates of a Proxmox VE cluster can be used, as one look
// found them.
type Sites struct {
	Storages  map[string]StorageSite
	Templates []TemplateSite
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
	inv, err := (&templates.Builder{Client: p.Client}).Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the Proxmox VE templates: %w", err)
	}
	for i, r := range inv.Templates {
		tags := templates.ParseTags(r.Tags)
		at, _ := templates.BuildOf(tags)
		s.Templates = append(s.Templates, TemplateSite{
			Name: r.Name, Node: r.Node, Tags: tags, Storages: inv.Sites[i].Storages, CPU: inv.Sites[i].CPU, Built: at,
		})
	}
	return s, nil
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
func (s *Sites) newest(line []string) []TemplateSite {
	key := templates.LineKey(line)
	var out []TemplateSite
	for _, t := range s.Templates {
		if templates.LineKey(t.Tags) != key {
			continue
		}
		switch {
		case len(out) == 0 || t.Built.After(out[0].Built):
			out = []TemplateSite{t}
		case t.Built.Equal(out[0].Built):
			out = append(out, t)
		}
	}
	return out
}

// reach are the nodes a template can be cloned on: those every one of its storages reaches. A
// storage is reached from every node it is available on when it is shared, otherwise only from the
// template's own node. ok is false for a template with no disk on a storage.
func (s *Sites) reach(t *TemplateSite) (nodes []string, ok bool) {
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
func (s *Sites) lineReach(line []string) (nodes []string, found []TemplateSite, constrained bool) {
	found = s.newest(line)
	for i := range found {
		reach, ok := s.reach(&found[i])
		if !ok {
			return nil, found, false
		}
		for _, n := range reach {
			if !slices.Contains(nodes, n) {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, found, len(found) > 0
}

// cpuNeed is the x86-64 level the cluster's nodes need of its hosts: the one its templates' own cpu
// setting presents when they set one, else fallback, the node OS's.
func (s *Sites) cpuNeed(c *clusterv1.Cluster, fallback int) int {
	need, set := 0, false
	for _, line := range templateLines(c) {
		for _, t := range s.newest(line) {
			if level, ok := templates.SettingLevel(t.CPU); ok {
				need, set = max(need, level), true
			}
		}
	}
	if !set {
		return fallback
	}
	return need
}

// Hosts are the names of the online nodes that can run the cluster's VMs: whose CPU reaches the
// level its templates run at, where its storage is available, and from which its templates can be
// cloned.
func (s *Sites) Hosts(c *clusterv1.Cluster, online []Node, fallbackLevel int) []string {
	hosts := nodesFor(online, s.cpuNeed(c, fallbackLevel))
	if storage, _, err := optionalVariable[string](c, "storage"); err == nil && storage != "" {
		if site, ok := s.Storages[storage]; ok {
			hosts = slices.DeleteFunc(hosts, func(n string) bool { return !slices.Contains(site.Active, n) })
		}
	}
	for _, line := range templateLines(c) {
		if reach, _, constrained := s.lineReach(line); constrained {
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
	if err := s.storageRefusal(c, names); err != nil {
		return err
	}
	for _, line := range templateLines(c) {
		reach, found, constrained := s.lineReach(line)
		if !constrained || slices.ContainsFunc(names, func(n string) bool { return slices.Contains(reach, n) }) {
			continue
		}
		return s.templateRefusal(&found[0], names)
	}
	return nil
}

func (s *Sites) storageRefusal(c *clusterv1.Cluster, names []string) error {
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
	return fmt.Errorf("storage %s is not available on any node this cluster may use (%s). It is available on %s. Choose another storage or allow one of those nodes",
		storage, strings.Join(names, ", "), strings.Join(site.Active, ", "))
}

func (s *Sites) templateRefusal(t *TemplateSite, names []string) error {
	for _, name := range t.Storages {
		if site, ok := s.Storages[name]; ok && site.Shared {
			continue
		}
		return fmt.Errorf("template %s is on storage %s, which is not shared, so it can be cloned only on node %s. None of the nodes this cluster may use (%s) is that node. "+
			"Put the templates on shared storage or allow node %s",
			t.Name, name, t.Node, strings.Join(names, ", "), t.Node)
	}
	return fmt.Errorf("template %s is on storage %s, which is not available on any node this cluster may use (%s). Bring the storage online on one of them or allow another node",
		t.Name, strings.Join(t.Storages, ", "), strings.Join(names, ", "))
}
