package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// SecurityGroupsVariable names the Proxmox VE security groups of the nodes' firewall, applied after
// the cluster's own traffic and before the serving ports. On a cluster without FirewallVariable,
// groups alone turn it on.
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
	// APIReach reads the firewalls' settings. It fails where the datacenter's cannot be read.
	APIReach(ctx context.Context) (*pve.APIReach, error)
}

// ManagementAddresses are the addresses the appliance and the management cluster's nodes reach
// Proxmox VE from; *FirewallSources is the real one.
type ManagementAddresses interface {
	Addresses(ctx context.Context) ([]string, error)
}

// APIReach implements APIReach. Every read needs Sys.Audit, which the controller's account holds
// on / when the setup asks for it.
func (p *Proxmox) APIReach(ctx context.Context) (*pve.APIReach, error) {
	r, err := pve.ReadAPIReach(ctx, p.Client.Get)
	if err != nil {
		return nil, fmt.Errorf("reading the Proxmox VE firewall: %w", err)
	}
	return r, nil
}

// refused tells whether Proxmox VE answered a request with a refusal for want of privileges, as
// opposed to giving no answer.
func refused(err error) bool {
	return errors.Is(err, proxmox.ErrNotAuthorized) || errors.Is(err, pve.ErrForbidden)
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

// validateFirewall holds a cluster's security groups whenever they or the firewall change: they
// need the firewall on, and they must be ones Proxmox VE lists, since either change replaces the
// nodes and Proxmox VE compiles a missing group as an empty one. Only on create or when the groups
// change must they not cost the management side the Proxmox VE API. The firewall alone is never
// held to that check: a VM's firewall leaves the nodes' own untouched. So a change Cluster API's
// defaulting makes, such as adding the firewall variable to an older cluster, passes it.
func (v *ClusterValidator) validateFirewall(ctx context.Context, old, c *clusterv1.Cluster) field.ErrorList {
	groupsChanged := old == nil || variableChanged(old, c, SecurityGroupsVariable)
	if !groupsChanged && !variableChanged(old, c, FirewallVariable) {
		return nil
	}
	// The class's schema refuses values that are malformed.
	groups, _, _ := optionalVariable[[]string](c, SecurityGroupsVariable)
	if len(groups) == 0 {
		return nil
	}
	if !firewalled(c) {
		path := field.NewPath("spec", "topology", "variables").Key(SecurityGroupsVariable)
		return field.ErrorList{field.Forbidden(path, "Security groups need the firewall on")}
	}
	var errs field.ErrorList
	if v.Hooks.SecurityGroups != nil {
		if existing, readable := v.Hooks.SecurityGroups.SecurityGroups(ctx); readable {
			errs = checkSecurityGroups(groups, existing)
		}
	}
	if !groupsChanged {
		return errs
	}
	return append(errs, v.validateAPIReach(ctx)...)
}

// validateAPIReach is checkAPIReach with what the hooks read. Security groups are named to act,
// which needs the datacenter firewall on, so they are held to the check also while it is off.
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
	reach, err := v.Hooks.APIReach.APIReach(ctx)
	if err != nil {
		return nil
	}
	return checkAPIReach(reach, management, endpoints)
}
