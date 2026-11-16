package ui

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// vmSet is the VMs in a cluster's pool. A machine's VM is the one with the guest ID capmox recorded
// on its ProxmoxMachine, whatever the VM's name now: a leftover VM may carry the name, or one may be
// renamed by hand. Proxmox VE lists a VM without a name while its node has dropped out of the
// statistics, and the ID finds that one too. Before capmox records an ID, as while it clones the
// VM, the VM is found by the name capmox gives it.
type vmSet struct {
	byID   map[int]pve.Member
	byName map[string]pve.Member
	// ids are the guest IDs on the ProxmoxMachines, by their names.
	ids map[string]int
	// unnamed are the VMs listed without a name, and claimed those of them a machine matched.
	unnamed int
	claimed map[int]bool
}

func (s *Server) newVMSet(ctx context.Context, kube client.Reader, c *clusterv1.Cluster, members []pve.Member) *vmSet {
	v := &vmSet{byID: make(map[int]pve.Member, len(members)), byName: make(map[string]pve.Member, len(members)), claimed: map[int]bool{}}
	for i := range members {
		m := members[i]
		if m.VMID != 0 {
			v.byID[m.VMID] = m
		}
		if m.Name != "" {
			v.byName[m.Name] = m
		} else if m.VMID != 0 {
			v.unnamed++
		}
	}
	if kube == nil {
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
	if id, recorded := v.ids[infra]; recorded {
		m, ok := v.byID[id]
		if !ok {
			return pve.Member{}, false
		}
		if m.Name == "" {
			v.claimed[m.VMID] = true
			m.Name = infra
		}
		return m, true
	}
	m, ok := v.byName[infra]
	return m, ok
}

// silent tells that a VM is listed without a name that no machine claimed, so a machine with no VM
// found may be the one whose node does not report.
func (v *vmSet) silent() bool {
	return v != nil && v.unnamed > len(v.claimed)
}
