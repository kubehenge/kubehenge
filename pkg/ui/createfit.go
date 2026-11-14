package ui

import (
	"fmt"
	"net/http"
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const (
	bytesPerMiB = 1 << 20
	bytesPerGiB = 1 << 30
)

// machinesFit refuses a cluster that could never run on the nodes and storage the user sees: a
// machine larger than the memory of every online node it may use, or disks beyond the free space
// of a storage that reserves their whole size. Memory merely in use now is left to the controller,
// whose NodesLackMemory condition has the cluster wait for room. Where a figure is not known,
// nothing is refused. It answers the request and returns false when it refuses.
func (s *Server) machinesFit(w http.ResponseWriter, r *http.Request, u user, class *clusterv1.ClusterClass, c *clusterv1.Cluster) bool {
	fit, err := provisioner.FitOf(c, class)
	if err != nil {
		// Admission says what is wrong with the sizes.
		return true
	}
	nodes, err := s.visibleNodes(r, u)
	if err != nil {
		s.proxmoxError(w, r, err)
		return false
	}
	usable := slices.DeleteFunc(nodes, func(n pve.Node) bool {
		return !n.Online || len(fit.Nodes) > 0 && !slices.Contains(fit.Nodes, n.Node)
	})
	if len(usable) > 0 && !slices.ContainsFunc(usable, func(n pve.Node) bool {
		return n.MaxMemory == 0 || n.MaxMemory >= fit.LargestMiB*bytesPerMiB
	}) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
			"machines: no node this cluster may use has %s of memory for its largest machine. Lower the machine's memory",
			provisioner.NeededGiB(fit.LargestMiB)))
		return false
	}
	if fit.Storage == "" {
		return true
	}
	storages, err := s.Accounts.Storages(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, r, err)
		return false
	}
	free, thick := freeOn(storages, fit.Storage, usable)
	if need := fit.DiskGiB * bytesPerGiB; thick && free >= 0 && need > free {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
			"machines: the disks need %d GiB and storage %s has %d GiB free. Lower a disk size or the node count",
			fit.DiskGiB, fit.Storage, free/bytesPerGiB))
		return false
	}
	return true
}

// freeOn is the free space of a storage on the usable nodes, once where it is shared, and whether
// it reserves a disk's whole size. The space is -1 where no node reports it.
func freeOn(storages []pve.Storage, name string, usable []pve.Node) (int64, bool) {
	free, thick, seen := int64(0), true, false
	for i := range storages {
		st := &storages[i]
		if st.Storage != name || st.Total == 0 ||
			!st.Shared && !slices.ContainsFunc(usable, func(n pve.Node) bool { return n.Node == st.Node }) {
			continue
		}
		onWrite, known := st.AllocatesOnWrite()
		thick = thick && known && !onWrite
		if st.Shared {
			return st.Avail, thick
		}
		free += st.Avail
		seen = true
	}
	if !seen {
		return -1, false
	}
	return free, thick
}
