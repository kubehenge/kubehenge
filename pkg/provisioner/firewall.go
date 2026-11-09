package provisioner

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// SecurityGroupsVariable names the Proxmox VE security groups of the nodes' firewall. Set, every
// node's VM firewall is on. It accepts the cluster's own traffic first, then applies the groups.
const SecurityGroupsVariable = "securityGroups"

// SecurityGroups lists the security groups of Proxmox VE's firewall; *Proxmox is the real one.
type SecurityGroups interface {
	// SecurityGroups lists the groups' names, and false where they cannot be read.
	SecurityGroups(ctx context.Context) ([]string, bool)
}

// SecurityGroups implements SecurityGroups. Proxmox VE lists the groups to every user.
func (p *Proxmox) SecurityGroups(ctx context.Context) ([]string, bool) {
	var groups []struct {
		Group string `json:"group"`
	}
	if err := p.Client.Get(ctx, "/cluster/firewall/groups", &groups); err != nil {
		return nil, false
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Group)
	}
	return names, true
}

// APIReach reads what the Proxmox VE firewalls let in on the API port; *Proxmox is the real one.
type APIReach interface {
	// APIReach reads the firewalls' settings, and false where the datacenter's cannot be read.
	APIReach(ctx context.Context) (*pve.APIReach, bool)
}

// ManagementAddresses are the addresses the appliance and the management cluster's nodes reach
// Proxmox VE from; *FirewallSources is the real one.
type ManagementAddresses interface {
	Addresses(ctx context.Context) ([]string, error)
}

// APIReach implements APIReach. Every read needs Sys.Audit, which the controller's account holds
// on / when the setup asks for it.
func (p *Proxmox) APIReach(ctx context.Context) (*pve.APIReach, bool) {
	r, err := pve.ReadAPIReach(ctx, p.Client.Get)
	return r, err == nil
}

// checkAPIReach refuses security groups that would cut the management side off the Proxmox VE API.
// They need the datacenter firewall, which turns each node's own firewall on. That one lets the API
// in only from the node's local network and the IPSet management, so an appliance or management
// cluster on another network could then no longer create or manage a VM. What cannot be told is
// let through, since Proxmox VE's own answer is not read here.
func checkAPIReach(reach *pve.APIReach, management []string, endpoints []netip.Addr) field.ErrorList {
	var addrs []netip.Addr
	for _, text := range management {
		// Only an address in a family of the endpoint reaches the API, so an IPv6 address of an
		// appliance that talks to an IPv4 endpoint is never cut off.
		ip, err := netip.ParseAddr(text)
		if err == nil && slices.ContainsFunc(endpoints, func(e netip.Addr) bool { return e.Is4() == ip.Unmap().Is4() }) {
			addrs = append(addrs, ip.Unmap())
		}
	}
	cut := reach.Cut(addrs).Cut
	if len(cut) == 0 {
		return nil
	}
	path := field.NewPath("spec", "topology", "variables").Key(SecurityGroupsVariable)
	return field.ErrorList{field.Invalid(path, SecurityGroupsVariable, pve.LockoutMessage(cut))}
}

// checkSecurityGroups refuses a security group Proxmox VE does not have. Proxmox VE's firewall skips
// a VM rule naming a missing group, so what the group was to accept would be dropped.
func checkSecurityGroups(groups, existing []string) field.ErrorList {
	path := field.NewPath("spec", "topology", "variables").Key(SecurityGroupsVariable)
	var errs field.ErrorList
	for i, group := range groups {
		if !slices.Contains(existing, group) {
			errs = append(errs, field.Invalid(path.Index(i), group,
				fmt.Sprintf("Proxmox VE has no security group %s. Create it under Datacenter, Firewall, Security Group", group)))
		}
	}
	return errs
}

// validateFirewall holds a cluster's security groups to those Proxmox VE has, when it is created and
// when they change, where Proxmox VE lists them.
func (v *ClusterValidator) validateFirewall(ctx context.Context, old, c *clusterv1.Cluster) field.ErrorList {
	if v.Hooks.SecurityGroups == nil || (old != nil && !variableChanged(old, c, SecurityGroupsVariable)) {
		return nil
	}
	groups, set, err := optionalVariable[[]string](c, SecurityGroupsVariable)
	if err != nil || !set || len(groups) == 0 {
		// The class's schema refuses groups that are malformed.
		return nil
	}
	existing, readable := v.Hooks.SecurityGroups.SecurityGroups(ctx)
	var errs field.ErrorList
	if readable {
		errs = checkSecurityGroups(groups, existing)
	}
	return append(errs, v.validateAPIReach(ctx)...)
}

// validateAPIReach is checkAPIReach with what the hooks read.
func (v *ClusterValidator) validateAPIReach(ctx context.Context) field.ErrorList {
	if v.Hooks.APIReach == nil || v.Hooks.Management == nil {
		return nil
	}
	endpoints, _ := pve.EndpointAddresses(ctx, v.Hooks.Endpoint)
	if len(endpoints) == 0 {
		return nil
	}
	management, err := v.Hooks.Management.Addresses(ctx)
	if err != nil || len(management) == 0 {
		return nil
	}
	reach, ok := v.Hooks.APIReach.APIReach(ctx)
	if !ok {
		return nil
	}
	return checkAPIReach(reach, management, endpoints)
}
