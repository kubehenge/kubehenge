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
	// Avail and Total are bytes; zero when the storage is not active on the node.
	Avail int64 `json:"avail"`
	Total int64 `json:"total"`
}

// Node is a Proxmox VE node as the user sees it.
type Node struct {
	Node   string `json:"node"`
	Online bool   `json:"online"`
}

// Bridge is a Linux or Open vSwitch bridge on a node.
type Bridge struct {
	Bridge    string `json:"bridge"`
	Node      string `json:"node"`
	VLANAware bool   `json:"vlanAware"`
	Comment   string `json:"comment,omitempty"`
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
	// Tags are separated by semicolons, as Proxmox VE stores them; Templates reads them, PoolMembers
	// cannot.
	Tags string `json:"tags"`
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

// Storages lists the storages on every node the user may see, with the content types each takes.
func (a *Accounts) Storages(ctx context.Context, t *Ticket) ([]Storage, error) {
	var res []struct {
		Storage string `json:"storage"`
		Node    string `json:"node"`
		Shared  int    `json:"shared"`
		Content string `json:"content"`
		Disk    int64  `json:"disk"`
		MaxDisk int64  `json:"maxdisk"`
		Status  string `json:"status"`
	}
	if err := a.get(ctx, t, "/cluster/resources?type=storage", &res); err != nil {
		return nil, err
	}
	var storages []Storage
	for _, r := range res {
		content := strings.Split(r.Content, ",")
		s := Storage{Storage: r.Storage, Node: r.Node, Shared: r.Shared != 0, Content: content}
		if r.Status == "available" {
			s.Avail, s.Total = r.MaxDisk-r.Disk, r.MaxDisk
		}
		storages = append(storages, s)
	}
	return storages, nil
}

// Nodes lists the nodes the user may see.
func (a *Accounts) Nodes(ctx context.Context, t *Ticket) ([]Node, error) {
	var res []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := a.get(ctx, t, "/cluster/resources?type=node", &res); err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(res))
	for _, r := range res {
		nodes = append(nodes, Node{Node: r.Node, Online: r.Status == "online"})
	}
	return nodes, nil
}

// Bridges lists a node's bridges; Proxmox VE shows them to users with Sys.Audit on the node.
func (a *Accounts) Bridges(ctx context.Context, t *Ticket, node string) ([]Bridge, error) {
	var res []struct {
		Iface     string `json:"iface"`
		VLANAware int    `json:"bridge_vlan_aware"`
		Comments  string `json:"comments"`
	}
	if err := a.get(ctx, t, "/nodes/"+url.PathEscape(node)+"/network?type=any_bridge", &res); err != nil {
		return nil, err
	}
	bridges := make([]Bridge, 0, len(res))
	for _, r := range res {
		bridges = append(bridges, Bridge{Bridge: r.Iface, Node: node, VLANAware: r.VLANAware != 0, Comment: strings.TrimSpace(r.Comments)})
	}
	return bridges, nil
}

// NodeAffinityRules lists the enabled HA node-affinity rules; Proxmox VE shows them to users with
// Sys.Audit on /.
func (a *Accounts) NodeAffinityRules(ctx context.Context, t *Ticket) ([]HARule, error) {
	// Proxmox VE leaves out a flag that is off, and writes one that is on as 1.
	var res []struct {
		Rule     string `json:"rule"`
		Comment  string `json:"comment"`
		Nodes    string `json:"nodes"`
		Affinity string `json:"affinity"`
		Strict   int    `json:"strict"`
		Disable  int    `json:"disable"`
	}
	if err := a.get(ctx, t, "/cluster/ha/rules?type=node-affinity", &res); err != nil {
		return nil, err
	}
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
	return rules, nil
}

// Zone returns the SDN zone of the bridge a VM connects to: a VNet's own zone, or localnetwork for
// a plain bridge, as Proxmox VE decides where SDN.Use is checked.
func (a *Accounts) Zone(ctx context.Context, t *Ticket, bridge string) (string, error) {
	var vnet struct {
		Zone string `json:"zone"`
	}
	err := a.get(ctx, t, "/cluster/sdn/vnets/"+url.PathEscape(bridge), &vnet)
	if notFound(err) {
		return "localnetwork", nil
	}
	if err != nil {
		return "", err
	}
	if vnet.Zone == "" {
		return "", fmt.Errorf("proxmox VE names no zone for VNet %s", bridge)
	}
	return vnet.Zone, nil
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

// Templates lists the VM templates in a pool, with their tags.
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
	for _, g := range guests {
		if g.Type == "qemu" && g.Template == 1 && g.Pool == pool {
			list = append(list, g.Member)
		}
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
	for _, m := range pools[0].Members {
		if m.Type == "qemu" {
			vms = append(vms, m)
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
	req.Header.Set("Cookie", "PVEAuthCookie="+t.Ticket)
	// Go sends every header name in this canonical form; Proxmox VE reads them regardless of case.
	req.Header.Set("Csrfpreventiontoken", t.CSRF)
	var upid string
	if err := a.do(req, &upid); err != nil {
		return err
	}
	return a.waitTask(ctx, t, node, upid)
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
	req.Header.Set("Cookie", "PVEAuthCookie="+t.Ticket)
	req.Header.Set("Csrfpreventiontoken", t.CSRF)
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
				return fmt.Errorf("proxmox VE task %s: %s", upid, st.ExitStatus)
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
