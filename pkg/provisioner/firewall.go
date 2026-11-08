package provisioner

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
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
	if !readable {
		return nil
	}
	return checkSecurityGroups(groups, existing)
}
