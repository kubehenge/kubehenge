package ui

import (
	"context"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// rebootReadyView says whether Proxmox VE hosts may be rebooted, as `kubehenge appliance
// ready-for-reboot` does. Answer is for the node the request names, or for all of them.
type rebootReadyView struct {
	provisioner.RebootReadiness

	Answer provisioner.RebootAnswer `json:"answer"`
}

// rebootReady answers whether every etcd member of the management cluster and the workload clusters
// is healthy and, with ?node=, whether that Proxmox VE node's guests can go away without costing an
// etcd its quorum. The CLI and the Administration page both read this one answer.
func (s *Server) rebootReady(w http.ResponseWriter, r *http.Request, u user) {
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	kube := s.currentKube()
	if kube == nil {
		writeError(w, http.StatusServiceUnavailable, "the management cluster is not set up yet")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), managementHealthTimeout)
	defer cancel()
	readiness, err := s.rebootReadiness(ctx, kube)
	if err != nil {
		s.Log.Warn("reading whether Proxmox VE hosts may reboot", "error", err)
		writeError(w, http.StatusBadGateway, "the management cluster does not answer: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rebootReadyView{RebootReadiness: readiness, Answer: readiness.For(r.URL.Query().Get("node"))})
}

// rebootReadiness joins the management cluster's etcd members, read now, with the record the
// controller keeps of the workload clusters'.
func (s *Server) rebootReadiness(ctx context.Context, kube client.Client) (provisioner.RebootReadiness, error) {
	management, err := provisioner.ManagementEtcd(ctx, kube, s.managementHosts())
	if err != nil {
		return provisioner.RebootReadiness{}, err //nolint:wrapcheck // names what it read
	}
	record, found, err := provisioner.ReadEtcdHealth(ctx, kube, s.ClassNamespace)
	if err != nil {
		return provisioner.RebootReadiness{}, err //nolint:wrapcheck // names what it read
	}
	return provisioner.ReadyForReboot(record, found, &management, s.now()), nil
}

// managementHosts are the Proxmox VE nodes the management cluster's VMs take in turn.
func (s *Server) managementHosts() []string {
	if s.Appliance != nil {
		if saved := s.Appliance.SavedSettings(); saved != nil {
			return saved.Nodes
		}
	}
	if s.demo != nil {
		return s.demo.hosts
	}
	return nil
}
