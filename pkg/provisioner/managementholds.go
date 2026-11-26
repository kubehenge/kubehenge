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
	holderManagementNode = "a management node's"
	holderManagementAPI  = "the management cluster's API"
	holderAppliance      = "the appliance's"
)

// HeldAddress is an address of the management side and who holds it, in the words of a refusal:
// "a management node's", "the management cluster's API" or "the appliance's".
type HeldAddress struct {
	Address netip.Addr
	Holder  string
}

// ManagementHolds are the addresses the management cluster and the appliance hold, and the bridge
// and VLAN they are on, as "<bridge>/<vlan>", which is empty where the setup did not say. A
// cluster's addresses on that network must leave them alone. *FirewallSources is the real one.
type ManagementHolds interface {
	Held(ctx context.Context) (string, []HeldAddress, error)
}

var _ ManagementHolds = &FirewallSources{}

// Held implements ManagementHolds: the management cluster's nodes, its API and the appliance, all
// on the management cluster's bridge and VLAN. Without a bridge it holds none, since the network
// the addresses are on is not known.
func (f *FirewallSources) Held(ctx context.Context) (string, []HeldAddress, error) {
	if f.Bridge == "" {
		return "", nil, nil
	}
	var held []HeldAddress
	add := func(text, holder string) {
		if a, err := netip.ParseAddr(strings.TrimSpace(text)); err == nil && a.Zone() == "" {
			held = append(held, HeldAddress{Address: a.Unmap(), Holder: holder})
		}
	}
	for _, a := range f.Management {
		add(a, holderManagementNode)
	}
	if u, err := url.Parse(f.API); err == nil && u.Hostname() != "" {
		add(u.Hostname(), holderManagementAPI)
	}
	own, err := f.applianceAddresses(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, a := range own {
		add(a, holderAppliance)
	}
	return fmt.Sprintf("%s/%d", f.Bridge, f.VLAN), held, nil
}

// checkManagementAddresses refuses node addresses, a control-plane endpoint or load-balancer
// addresses of a cluster on the management cluster's bridge and VLAN that include an address the
// management side holds: two hosts answering for one address take each other's traffic, and a
// node that gets an address another host holds never joins.
func checkManagementAddresses(cluster *clusterv1.Cluster, segment string, held []HeldAddress) error {
	if segment == "" || len(held) == 0 {
		return nil
	}
	own, err := addressesOf(cluster)
	if err != nil || own.segment != segment {
		// What does not read is refused by checkAddresses.
		return nil //nolint:nilerr // the other check names it
	}
	var problems []string
	for _, h := range held {
		a := addressSpan{h.Address, h.Address}
		for _, m := range own.machines {
			if m.overlaps(a) {
				problems = append(problems, fmt.Sprintf("node addresses %s include %s, which is %s address", m, h.Address, h.Holder))
			}
		}
		if own.endpoint.overlaps(a) {
			problems = append(problems, fmt.Sprintf("control-plane endpoint %s is %s address", own.endpoint, h.Holder))
		}
		for _, s := range own.services {
			if s.overlaps(a) {
				problems = append(problems, fmt.Sprintf("load-balancer addresses %s include %s, which is %s address", s, h.Address, h.Holder))
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
	segment, held, err := holds.Held(ctx)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "reading the addresses the management side holds")
		return nil
	}
	return checkManagementAddresses(cluster, segment, held)
}
