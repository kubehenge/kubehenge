package ui

import (
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// machineOf describes one of a cluster's machines, on the Proxmox VE node its VM runs on.
func machineOf(m *clusterv1.Machine, proxmoxNode string) machineView {
	mv := machineView{
		Name:        m.Name,
		Node:        m.Status.NodeRef.Name,
		ProxmoxNode: proxmoxNode,
		Role:        workerRole,
		Version:     m.Spec.Version,
		Phase:       m.Status.Phase,
		Ready:       meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineReadyCondition),
		Created:     m.CreationTimestamp.Time,
	}
	if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; cp {
		mv.Role = controlPlaneRole
	}
	if ready := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineReadyCondition); ready != nil && !mv.Ready {
		mv.Waiting = ready.Message
	}
	if _, marked := m.Annotations[clusterv1.RemediateMachineAnnotation]; marked {
		mv.Replacing = true
		// What holds the replacement back, such as a control plane that would lose etcd's majority.
		if owner := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineOwnerRemediatedCondition); owner != nil && owner.Message != "" {
			mv.Waiting = owner.Message
		}
	}
	for _, a := range m.Status.Addresses {
		if a.Type == clusterv1.MachineInternalIP || a.Type == clusterv1.MachineExternalIP {
			mv.Addresses = append(mv.Addresses, a.Address)
		}
	}
	return mv
}

// replaceMachine marks one of a cluster's machines for its health check to replace, as
// `kubectl annotate machine <name> cluster.x-k8s.io/remediate-machine=` does. Cluster API then
// replaces it as it would a node that stays not ready: workers one at a time, and a control plane
// only while etcd keeps its majority.
func (s *Server) replaceMachine(w http.ResponseWriter, r *http.Request, u user) {
	s.patchMachine(w, r, u, fmt.Appendf(nil, `{"metadata":{"annotations":{%q:""}}}`, clusterv1.RemediateMachineAnnotation))
}

// keepMachine takes the mark back, for a machine Cluster API has not begun to replace.
func (s *Server) keepMachine(w http.ResponseWriter, r *http.Request, u user) {
	s.patchMachine(w, r, u, fmt.Appendf(nil, `{"metadata":{"annotations":{%q:null}}}`, clusterv1.RemediateMachineAnnotation))
}

// patchMachine applies patch to the machine the request names, which must be one of its cluster's.
func (s *Server) patchMachine(w http.ResponseWriter, r *http.Request, u user, patch []byte) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	m := &clusterv1.Machine{}
	err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("machine")}, m)
	if err != nil && !apierrors.IsNotFound(err) {
		s.kubeError(w, err)
		return
	}
	if err != nil || m.Labels[clusterv1.ClusterNameLabel] != c.Name {
		writeError(w, http.StatusNotFound, "no such machine")
		return
	}
	if err := u.kube.Patch(r.Context(), m, client.RawPatch(types.MergePatchType, patch)); err != nil {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("machine changed", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "machine", m.Name, "patch", string(patch))
	w.WriteHeader(http.StatusNoContent)
}
