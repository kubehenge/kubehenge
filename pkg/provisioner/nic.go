package provisioner

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// The bounds of a node's network interface options. Proxmox VE takes an MTU from 576, but capmox's
// webhook refuses one under 1280 because IPv6 does not work below it. Proxmox VE takes at most 64
// queues on a virtio interface.
const (
	MinNodeMTU   = 1280
	MaxNodeMTU   = 65520
	MaxNodeQueue = 64
	// defaultBridgeMTU is what a Linux bridge has where Proxmox VE lists none.
	defaultBridgeMTU = 1500
)

// BridgeMTUs reads the MTU of Proxmox VE bridges; *Proxmox is the real one.
type BridgeMTUs interface {
	// BridgeMTU is the smallest MTU the bridge has on those of the given nodes that are online (on
	// every online node when none are given), and false where it cannot be read.
	BridgeMTU(ctx context.Context, bridge string, nodes []string) (int, bool)
}

// pveNumber reads a number Proxmox VE sends as a string or as a number.
type pveNumber int

func (n *pveNumber) UnmarshalJSON(raw []byte) error {
	v, err := strconv.Atoi(strings.Trim(string(raw), `"`))
	if err != nil {
		return fmt.Errorf("reading %s as a number: %w", raw, err)
	}
	*n = pveNumber(v)
	return nil
}

// BridgeMTU implements BridgeMTUs. A node whose network cannot be read, such as one the controller's
// account may not audit, is left out.
func (p *Proxmox) BridgeMTU(ctx context.Context, bridge string, nodes []string) (int, bool) {
	var listed []pveNode
	if err := p.Client.Get(ctx, "/nodes", &listed); err != nil {
		return 0, false
	}
	smallest, found := 0, false
	for _, n := range listed {
		if n.Status != nodeOnline || (len(nodes) > 0 && !slices.Contains(nodes, n.Node)) {
			continue
		}
		var ifaces []struct {
			Iface string    `json:"iface"`
			MTU   pveNumber `json:"mtu"`
		}
		if err := p.Client.Get(ctx, "/nodes/"+url.PathEscape(n.Node)+"/network?type=any_bridge", &ifaces); err != nil {
			continue
		}
		for _, i := range ifaces {
			if i.Iface != bridge {
				continue
			}
			mtu := cmp.Or(int(i.MTU), defaultBridgeMTU)
			if !found || mtu < smallest {
				smallest = mtu
			}
			found = true
		}
	}
	return smallest, found
}

// checkNodeNIC holds the interface options of a cluster's nodes to Proxmox VE's and capmox's bounds.
func checkNodeNIC(n nodeNetwork) field.ErrorList {
	path := field.NewPath("spec", "topology", "variables").Key("network")
	var errs field.ErrorList
	if n.MTU != nil && (*n.MTU < MinNodeMTU || *n.MTU > MaxNodeMTU) {
		errs = append(errs, field.Invalid(path.Child("mtu"), *n.MTU, fmt.Sprintf("MTU: %d to %d", MinNodeMTU, MaxNodeMTU)))
	}
	if n.Queues != nil && (*n.Queues < 1 || *n.Queues > MaxNodeQueue) {
		errs = append(errs, field.Invalid(path.Child("queues"), *n.Queues, fmt.Sprintf("Queues: 1 to %d", MaxNodeQueue)))
	}
	return errs
}

// checkBridgeMTU refuses an interface MTU above the bridge's: its larger frames never pass.
func checkBridgeMTU(n nodeNetwork, bridge int) *field.Error {
	if n.MTU == nil || *n.MTU <= bridge {
		return nil
	}
	return field.Invalid(field.NewPath("spec", "topology", "variables").Key("network").Child("mtu"), *n.MTU,
		fmt.Sprintf("The bridge %s has MTU %d. Use %d or less, or raise the bridge's MTU", n.Bridge, bridge, bridge))
}

// validateNodeNIC holds a cluster to its nodes' interface options when it is created and when its
// network changes: the bounds, then the bridge's MTU where Proxmox VE tells it.
func (v *ClusterValidator) validateNodeNIC(ctx context.Context, old, c *clusterv1.Cluster) field.ErrorList {
	if old != nil && !variableChanged(old, c, "network") {
		return nil
	}
	n, set, err := optionalVariable[nodeNetwork](c, "network")
	if err != nil || !set {
		// The class's schema refuses a network that is missing or malformed.
		return nil
	}
	if errs := checkNodeNIC(n); len(errs) > 0 || n.MTU == nil || v.Hooks.Bridges == nil {
		return errs
	}
	allowed, _, _ := optionalVariable[[]string](c, allowedNodesVariable)
	bridge, readable := v.Hooks.Bridges.BridgeMTU(ctx, n.Bridge, allowed)
	if !readable {
		return nil
	}
	if err := checkBridgeMTU(n, bridge); err != nil {
		return field.ErrorList{err}
	}
	return nil
}
