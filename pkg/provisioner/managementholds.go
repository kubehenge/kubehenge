package provisioner

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// The holders of the addresses the management side keeps, as an address's refusal names them.
const (
	holderManagementNode = "a management node's address"
	holderManagementAPI  = "the management cluster's API address"
	holderAppliance      = "the appliance's address"
)

// nodeHolder names a management node, or says only that the address is a management node's where
// the node's name is not known.
func nodeHolder(name string) string {
	if name == "" {
		return holderManagementNode
	}
	return "management node " + name
}

// HeldAddress is an address of the management side, who holds it, in the words of a refusal
// ("management node <name>", "the management cluster's API address" or "the appliance's address"),
// and the bridge and VLAN it is on, as "<bridge>/<vlan>".
type HeldAddress struct {
	Address netip.Addr
	Holder  string
	Segment string
}

// ManagementHolds are the addresses the management cluster and the appliance hold, each on its
// bridge and VLAN. A cluster's addresses on that network must leave them alone. *FirewallSources is
// the real one.
type ManagementHolds interface {
	Held(ctx context.Context) ([]HeldAddress, error)
}

var _ ManagementHolds = &FirewallSources{}

// Held implements ManagementHolds: the management cluster's nodes and its API on the management
// cluster's bridge and VLAN, and the appliance on the network it recorded for itself. An appliance
// that recorded none is taken to be on the management cluster's. What is on a network that is not
// known is not held.
func (f *FirewallSources) Held(ctx context.Context) ([]HeldAddress, error) {
	management := ""
	if f.Bridge != "" {
		management = fmt.Sprintf("%s/%d", f.Bridge, f.VLAN)
	}
	var held []HeldAddress
	add := func(text, holder, segment string) {
		if a, err := netip.ParseAddr(strings.TrimSpace(text)); err == nil && a.Zone() == "" && segment != "" {
			held = append(held, HeldAddress{Address: a.Unmap(), Holder: holder, Segment: segment})
		}
	}
	for _, a := range f.Management {
		add(a, nodeHolder(f.Nodes[a]), management)
	}
	if u, err := url.Parse(f.API); err == nil && u.Hostname() != "" {
		add(u.Hostname(), holderManagementAPI, management)
	}
	own, segment, err := f.appliance(ctx)
	if err != nil {
		return nil, err
	}
	if segment == "" {
		segment = management
	}
	for _, a := range own {
		add(a, holderAppliance, segment)
	}
	return held, nil
}

// checkManagementAddresses refuses node addresses, a control-plane endpoint or load-balancer
// addresses of a cluster that include an address the management side holds on the cluster's bridge
// and VLAN: two hosts answering for one address take each other's traffic, and a node that gets an
// address another host holds never joins.
func checkManagementAddresses(cluster *clusterv1.Cluster, held []HeldAddress) error {
	if len(held) == 0 {
		return nil
	}
	own, err := addressesOf(cluster)
	if err != nil {
		// What does not read is refused by checkAddresses.
		return nil //nolint:nilerr // the other check names it
	}
	var problems []string
	for _, h := range held {
		if h.Segment != own.segment {
			continue
		}
		a := addressSpan{h.Address, h.Address}
		for _, m := range own.machines {
			if m.overlaps(a) {
				problems = append(problems, fmt.Sprintf("node addresses %s include %s, which is %s", m, h.Address, h.Holder))
			}
		}
		if own.endpoint.overlaps(a) {
			problems = append(problems, fmt.Sprintf("control-plane endpoint %s is %s", own.endpoint, h.Holder))
		}
		for _, s := range own.services {
			if s.overlaps(a) {
				problems = append(problems, fmt.Sprintf("load-balancer addresses %s include %s, which is %s", s, h.Address, h.Holder))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, ", "))
}

// checkManagement holds the cluster's addresses to the ones the management side holds, where the
// hooks know them. A failed read lets the cluster through, since it would otherwise stop every
// creation while the management cluster's ConfigMap cannot be read.
func (h *Hooks) checkManagement(ctx context.Context, cluster *clusterv1.Cluster) error {
	holds, ok := h.Management.(ManagementHolds)
	if !ok {
		return nil
	}
	held, err := holds.Held(ctx)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "reading the addresses the management side holds")
		return nil
	}
	return checkManagementAddresses(cluster, held)
}
