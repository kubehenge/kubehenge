package ui

import (
	"net/http"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// recoveryView is how the control plane of a cluster whose roll stalled can be recovered: by the
// controller, or by hand with the plan's steps.
type recoveryView struct {
	Automatic bool                         `json:"automatic"`
	Plan      *provisioner.EtcdRestorePlan `json:"plan,omitempty"`
}

// recoveryRequest names the control-plane machine whose etcd is kept, and the cluster's name typed
// to confirm it.
type recoveryRequest struct {
	Machine string `json:"machine"`
	Confirm string `json:"confirm"`
}

// controlPlaneRecovery says how the cluster's control plane is recovered keeping the machine named
// in ?machine=: automatically, or by the steps of a plan.
func (s *Server) controlPlaneRecovery(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	machine := r.URL.Query().Get("machine")
	if !s.isControlPlaneMachine(w, r, u, c, machine) {
		return
	}
	automatic, err := s.automaticRestore(r.Context(), u, c)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	view := recoveryView{Automatic: automatic}
	if !automatic {
		plan, err := provisioner.PlanControlPlaneReset(r.Context(), u.kube, c, machine)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		view.Plan = plan
	}
	writeJSON(w, http.StatusOK, view)
}

// recoverControlPlane asks the controller to reset the cluster's etcd to one control-plane
// machine's own and replace the others, as the way out of a roll that stalled. It takes what an
// etcd restore takes, Sys.Modify on the cluster's pool, and the cluster's name typed: whatever the
// other machines' etcd held beyond the kept one's is gone.
func (s *Server) recoverControlPlane(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	var req recoveryRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != c.Name {
		writeError(w, http.StatusUnprocessableEntity, "type the cluster's name to confirm")
		return
	}
	if provisioner.RestoreInProgress(c) {
		writeError(w, http.StatusConflict, "the cluster's etcd is being restored already")
		return
	}
	if !meta.IsStatusConditionTrue(c.Status.Conditions, provisioner.RollStalledCondition) {
		writeError(w, http.StatusConflict, "the control plane's roll has not stalled")
		return
	}
	if !s.isControlPlaneMachine(w, r, u, c, req.Machine) {
		return
	}
	automatic, err := s.automaticRestore(r.Context(), u, c)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	if !automatic {
		writeError(w, http.StatusConflict, "no guest agent access: recover by hand with the steps")
		return
	}
	request := provisioner.EtcdRestoreRequest{Machine: req.Machine, By: u.ticket.Username, At: time.Now().UTC()}
	if !s.annotate(w, r, u, c, provisioner.EtcdRestoreAnnotation, request) {
		return
	}
	s.Log.Info("asked to recover a control plane", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "keep", req.Machine)
	writeJSON(w, http.StatusAccepted, request)
}

// isControlPlaneMachine answers 422 unless name is one of the cluster's control-plane machines
// that is not being deleted.
func (s *Server) isControlPlaneMachine(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, name string) bool {
	var machines clusterv1.MachineList
	err := u.kube.List(r.Context(), &machines, client.InNamespace(c.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		s.kubeError(w, r, err)
		return false
	}
	if !slices.ContainsFunc(machines.Items, func(m clusterv1.Machine) bool { return m.Name == name && m.DeletionTimestamp.IsZero() }) {
		writeError(w, http.StatusUnprocessableEntity, "not a control-plane machine of this cluster")
		return false
	}
	return true
}
