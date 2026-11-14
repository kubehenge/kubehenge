package provisioner

import (
	"context"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// validateVLAN refuses a VLAN tag on a VNet whose zone tags its traffic already, when the cluster
// is created or its bridge or tag change. A VNet whose zone the token cannot read is not judged.
func (v *ClusterValidator) validateVLAN(ctx context.Context, reads *pveReads, old, c *clusterv1.Cluster) field.ErrorList {
	if v.Hooks.Sites == nil {
		return nil
	}
	n, set, err := optionalVariable[nodeNetwork](c, networkVariable)
	if err != nil || !set || n.VLAN <= 0 {
		return nil
	}
	if old != nil {
		if was, _, _ := optionalVariable[nodeNetwork](old, networkVariable); was.Bridge == n.Bridge && was.VLAN == n.VLAN {
			return nil
		}
	}
	sites, err := reads.lookAtSites(ctx)
	if err != nil {
		_ = admitOnReadFailure(ctx, err)
		return nil
	}
	if !sites.TaggingVNets[n.Bridge] {
		return nil
	}
	path := field.NewPath("spec", "topology", "variables").Key(networkVariable).Child("vlan")
	return field.ErrorList{field.Invalid(path, n.VLAN, pve.VLANOnTaggedNetwork(n.Bridge))}
}
