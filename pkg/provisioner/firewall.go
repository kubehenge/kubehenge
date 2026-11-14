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

// SecurityGroupsVariable names the Proxmox VE security groups of the nodes' firewall, applied after
// the firewall's own rules. On a cluster without FirewallVariable, groups alone turn it on.
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

// checkAPIReach refuses a firewall that would cut the management side off the Proxmox VE API. It
// needs the datacenter firewall, which turns each node's own firewall on. That one lets the API
// in only from the node's local network and the IPSet management, so an appliance or management
// cluster on another network could then no longer create or manage a VM. What cannot be told is
// let through, since Proxmox VE's own answer is not read here.
func checkAPIReach(reach *pve.APIReach, management []string, endpoints []netip.Addr, variable string) field.ErrorList {
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
	path := field.NewPath("spec", "topology", "variables").Key(variable)
	return field.ErrorList{field.Invalid(path, variable, pve.LockoutMessage(cut))}
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

// validateFirewall holds a firewalled cluster, when it is created and when its firewall or groups
// change: groups need the firewall on and must be ones Proxmox VE lists, and the management side
// must keep the Proxmox VE API once the datacenter firewall is on.
func (v *ClusterValidator) validateFirewall(ctx context.Context, old, c *clusterv1.Cluster) field.ErrorList {
	if old != nil && !variableChanged(old, c, SecurityGroupsVariable) && !variableChanged(old, c, FirewallVariable) {
		return nil
	}
	// The class's schema refuses values that are malformed.
	groups, _, _ := optionalVariable[[]string](c, SecurityGroupsVariable)
	if !firewalled(c) {
		if len(groups) == 0 {
			return nil
		}
		path := field.NewPath("spec", "topology", "variables").Key(SecurityGroupsVariable)
		return field.ErrorList{field.Forbidden(path, "Security groups need the firewall on")}
	}
	var errs field.ErrorList
	if len(groups) > 0 && v.Hooks.SecurityGroups != nil {
		if existing, readable := v.Hooks.SecurityGroups.SecurityGroups(ctx); readable {
			errs = checkSecurityGroups(groups, existing)
		}
	}
	// Groups are named to act, which needs the datacenter firewall on, so they are held to the
	// check also while it is off. The firewall alone is held to it once the datacenter's is on.
	return append(errs, v.validateAPIReach(ctx, len(groups) > 0)...)
}

// validateAPIReach is checkAPIReach with what the hooks read. Unless always, it holds only while the
// datacenter firewall is on.
func (v *ClusterValidator) validateAPIReach(ctx context.Context, always bool) field.ErrorList {
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
	if !ok || (!always && !reach.Enabled) {
		return nil
	}
	variable := FirewallVariable
	if always {
		variable = SecurityGroupsVariable
	}
	return checkAPIReach(reach, management, endpoints, variable)
}
