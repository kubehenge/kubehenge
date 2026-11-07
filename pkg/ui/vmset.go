package ui

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// vmSet is the VMs in a cluster's pool, found by the name the Proxmox VE provider gives a machine's
// VM. Proxmox VE lists a VM without a name while its node has dropped out of the statistics, so such
// a VM is found by the guest ID capmox recorded on its machine's ProxmoxMachine.
type vmSet struct {
	byName map[string]pve.Member
	// unnamed are the VMs listed without a name, by guest ID.
	unnamed map[int]pve.Member
	// ids are the guest IDs on the ProxmoxMachines, by their names.
	ids     map[string]int
	claimed map[int]bool
}

func (s *Server) newVMSet(ctx context.Context, kube client.Reader, c *clusterv1.Cluster, members []pve.Member) *vmSet {
	v := &vmSet{byName: make(map[string]pve.Member, len(members)), unnamed: map[int]pve.Member{}, claimed: map[int]bool{}}
	for i := range members {
		switch m := members[i]; {
		case m.Name != "":
			v.byName[m.Name] = m
		case m.VMID != 0:
			v.unnamed[m.VMID] = m
		}
	}
	if len(v.unnamed) == 0 || kube == nil {
		return v
	}
	ids, err := provisioner.ProxmoxMachineVMIDs(ctx, kube, c.Namespace, c.Name)
	if err != nil {
		s.Log.WarnContext(ctx, "reading a cluster's VM IDs", "cluster", c.Namespace+"/"+c.Name, "error", err)
	}
	v.ids = ids
	return v
}

// of finds the VM of the machine whose ProxmoxMachine is named infra.
func (v *vmSet) of(infra string) (pve.Member, bool) {
	if v == nil || infra == "" {
		return pve.Member{}, false
	}
	if m, ok := v.byName[infra]; ok {
		return m, true
	}
	m, ok := v.unnamed[v.ids[infra]]
	if !ok {
		return pve.Member{}, false
	}
	v.claimed[m.VMID] = true
	m.Name = infra
	return m, true
}

// silent tells that a VM is listed without a name that no machine claimed, so a machine with no VM
// found may be the one whose node does not report.
func (v *vmSet) silent() bool {
	return v != nil && len(v.unnamed) > len(v.claimed)
}
