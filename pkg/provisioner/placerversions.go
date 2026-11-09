package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// vmMTUs is implemented by a Nodes that can tell the interface size a pool's VMs run with.
type vmMTUs interface {
	// NodeMTUs gives the smallest interface MTU of the pool's VMs on each node that runs one.
	NodeMTUs(ctx context.Context, pool string, online []Node) (map[string]int, error)
}

// annotate sets an annotation on the cluster, or removes it when value is empty, and does nothing
// when the cluster holds it already.
func (p *Placer) annotate(ctx context.Context, c *clusterv1.Cluster, key, value string) {
	if c.Annotations[key] == value {
		return
	}
	var v *string
	if value != "" {
		v = &value
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: map[string]any{key: v}}})
	if err != nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "encoding an annotation", "annotation", key)
		return
	}
	target := &clusterv1.Cluster{}
	target.Namespace, target.Name = c.Namespace, c.Name
	if err := p.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "recording an annotation", "annotation", key, "cluster", client.ObjectKeyFromObject(c))
		}
	}
}

// markVersions records on the cluster which Proxmox VE nodes run which major version while they
// differ.
func (p *Placer) markVersions(ctx context.Context, c *clusterv1.Cluster, online []Node) {
	p.annotate(ctx, c, ProxmoxVersionsAnnotation, versionNote(online))
}

// clearNewer notes that the cluster's placement holds no node of a newer Proxmox VE version for want
// of room among the older ones.
func (p *Placer) clearNewer(c *clusterv1.Cluster) {
	delete(p.newer, client.ObjectKeyFromObject(c).String())
}

// sayNewer says in an event, once each time it starts, that the cluster's placement holds nodes of a
// newer Proxmox VE version because none of the older nodes has room.
func (p *Placer) sayNewer(c *clusterv1.Cluster, online []Node) {
	key := client.ObjectKeyFromObject(c).String()
	was := p.newer[key]
	if p.newer == nil {
		p.newer = map[string]bool{}
	}
	p.newer[key] = true
	if was || p.Events == nil {
		return
	}
	p.Events.Eventf(c, nil, corev1.EventTypeWarning, "ProxmoxVersionsMixed", "Place",
		"No older Proxmox VE node has room for the cluster's machines (%s), so new VMs may start on a newer node. A VM started there cannot move back to an older node",
		strings.ReplaceAll(strings.ReplaceAll(versionNote(online), "=", ": "), ";", ", "))
}

// markMTUs records the interface sizes of the cluster's VMs per node while they differ.
func (p *Placer) markMTUs(ctx context.Context, c *clusterv1.Cluster, online []Node) {
	reader, ok := p.Nodes.(vmMTUs)
	if !ok || p.WorkloadPool == "" {
		return
	}
	names, err := NamesFor(p.WorkloadPool, c.Namespace, c.Name)
	if err != nil {
		return
	}
	mtus, err := reader.NodeMTUs(ctx, names.Pool, online)
	if err != nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "reading the cluster's VM interface sizes", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	p.annotate(ctx, c, NodeMTUsAnnotation, mtuNote(mtus))
}

// mtuNote is NodeMTUsAnnotation's value: empty while every node's VMs have the same MTU.
func mtuNote(mtus map[string]int) string {
	distinct := map[int]bool{}
	for _, m := range mtus {
		distinct[m] = true
	}
	if len(distinct) <= 1 {
		return ""
	}
	nodes := make([]string, 0, len(mtus))
	for n := range mtus {
		nodes = append(nodes, n)
	}
	slices.Sort(nodes)
	pairs := make([]string, len(nodes))
	for i, n := range nodes {
		pairs[i] = n + "=" + strconv.Itoa(mtus[n])
	}
	return strings.Join(pairs, ",")
}

// NodeMTU is one node's smallest VM interface MTU.
type NodeMTU struct {
	Node string
	MTU  int
}

// ParseNodeMTUs reads NodeMTUsAnnotation.
func ParseNodeMTUs(note string) []NodeMTU {
	var out []NodeMTU
	for pair := range strings.SplitSeq(note, ",") {
		node, value, ok := strings.Cut(pair, "=")
		mtu, err := strconv.Atoi(value)
		if ok && err == nil && node != "" {
			out = append(out, NodeMTU{Node: node, MTU: mtu})
		}
	}
	return out
}

// NodeMTUs implements vmMTUs. The guest agent's network-get-interfaces carries no MTU, so the size
// is what Proxmox VE gives the VM's first interface: its own mtu setting, otherwise the bridge's
// MTU on Proxmox VE 9 and 1500 before. A VM that was running before an upgrade may hold the old
// size until it restarts, which this does not see.
func (p *Proxmox) NodeMTUs(ctx context.Context, pool string, online []Node) (map[string]int, error) {
	var vms []struct {
		VMID     int64     `json:"vmid"`
		Node     string    `json:"node"`
		Pool     string    `json:"pool"`
		Type     string    `json:"type"`
		Template pveNumber `json:"template"`
	}
	if err := p.Client.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return nil, fmt.Errorf("listing the VMs: %w", err)
	}
	major := map[string]int{}
	for _, n := range online {
		major[n.Name] = n.Major
	}
	bridges := map[string]int{}
	out := map[string]int{}
	for _, vm := range vms {
		if vm.Pool != pool || vm.Type != "qemu" || vm.Template != 0 {
			continue
		}
		var config map[string]any
		path := "/nodes/" + url.PathEscape(vm.Node) + "/qemu/" + strconv.FormatInt(vm.VMID, 10) + "/config"
		if err := p.Client.Get(ctx, path, &config); err != nil {
			continue
		}
		net0, ok := config["net0"].(string)
		if !ok {
			continue
		}
		bridge, set := net0Settings(net0)
		mtu := set
		if mtu <= 1 {
			mtu = defaultBridgeMTU
			if set == 1 || major[vm.Node] >= newestBridgeMTUMajor {
				mtu = p.cachedBridgeMTU(ctx, bridges, vm.Node, bridge)
			}
		}
		if have, ok := out[vm.Node]; !ok || mtu < have {
			out[vm.Node] = mtu
		}
	}
	return out, nil
}

// newestBridgeMTUMajor is the first Proxmox VE major version whose unset interface MTU follows the
// bridge's.
const newestBridgeMTUMajor = 9

// net0Settings reads the bridge and the mtu setting (0 when unset) of a VM's first interface.
func net0Settings(net0 string) (string, int) {
	bridge, set := "", 0
	for part := range strings.SplitSeq(net0, ",") {
		switch key, value, _ := strings.Cut(part, "="); key {
		case "bridge":
			bridge = value
		case "mtu":
			set, _ = strconv.Atoi(value)
		}
	}
	return bridge, set
}

// cachedBridgeMTU is the bridge's MTU on the node, read once per call into known.
func (p *Proxmox) cachedBridgeMTU(ctx context.Context, known map[string]int, node, bridge string) int {
	key := node + "/" + bridge
	if m, ok := known[key]; ok {
		return m
	}
	var ifaces []struct {
		Iface string    `json:"iface"`
		MTU   pveNumber `json:"mtu"`
	}
	m := defaultBridgeMTU
	if err := p.Client.Get(ctx, "/nodes/"+url.PathEscape(node)+"/network?type=any_bridge", &ifaces); err == nil {
		for _, i := range ifaces {
			if i.Iface == bridge {
				m = cmp.Or(int(i.MTU), defaultBridgeMTU)
			}
		}
	}
	known[key] = m
	return m
}
