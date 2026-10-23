package provisioner

import (
	"errors"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// ccmVariable configures a cluster's Proxmox VE cloud controller manager.
const ccmVariable = "ccm"

// CCM is a cluster's ccm variable.
type CCM struct {
	// HAGroups gives the cluster's token Sys.Audit on /, unpropagated. The cloud controller manager
	// labels each node with the Proxmox VE HA groups its Proxmox VE node is in, or with the
	// node-affinity HA rules once Proxmox VE has turned the groups into rules, and Proxmox VE lists
	// either only with Sys.Audit on /.
	HAGroups bool `json:"haGroups,omitempty"`
}

// ccmOf reads a cluster's ccm variable, the zero value if it has none.
func ccmOf(cluster *clusterv1.Cluster) (CCM, error) {
	c, _, err := optionalVariable[CCM](cluster, ccmVariable)
	return c, err
}

// sysAudit tells whether the cluster's user is to hold Sys.Audit on /, and why not when its ccm
// variable asks for it and something stands in the way. group is the Proxmox VE group that holds
// it, empty when Kubehenge's administrator lets no cluster hold it.
func sysAudit(cluster *clusterv1.Cluster, group string) (bool, string, error) {
	ccm, err := ccmOf(cluster)
	switch {
	case err != nil || !ccm.HAGroups:
		return false, "", err
	case group == "":
		return false, "Kubehenge's administrator lets no cluster hold Sys.Audit on /, which the cloud controller manager's HA group labels need", nil
	}
	return true, "", nil
}

// checkSysAudit refuses Sys.Audit on / for a cluster where no cluster may hold it.
func (h *Hooks) checkSysAudit(cluster *clusterv1.Cluster) error {
	_, refused, err := sysAudit(cluster, h.SysAuditGroup)
	if err != nil {
		return err
	}
	if refused != "" {
		return errors.New(refused)
	}
	return nil
}
