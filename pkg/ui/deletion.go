package ui

import (
	"encoding/json"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// abandonRequest is the cluster's name typed to confirm that its deletion is abandoned.
type abandonRequest struct {
	Confirm string `json:"confirm"`
}

// abandonDeletion has the controller give up on what a cluster's deletion waits for, as the
// annotation does through kubectl. It takes what deleting the cluster takes, and the cluster's name
// typed: what does not finish deleting is left behind in Proxmox VE.
func (s *Server) abandonDeletion(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	var req abandonRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != c.Name {
		writeError(w, http.StatusUnprocessableEntity, "type the cluster's name to confirm")
		return
	}
	if c.DeletionTimestamp.IsZero() {
		writeError(w, http.StatusConflict, "the cluster is not being deleted")
		return
	}
	if provisioner.Abandoned(c) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{provisioner.AbandonAnnotation: c.Name}}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the request could not be encoded")
		return
	}
	if err := u.kube.Patch(r.Context(), c, client.RawPatch("application/merge-patch+json", patch)); err != nil {
		s.kubeError(w, r, err)
		return
	}
	s.Log.Info("abandoned a cluster's deletion", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	w.WriteHeader(http.StatusAccepted)
}
