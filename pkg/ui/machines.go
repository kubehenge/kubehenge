package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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
	} else {
		mv.Pool = m.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel]
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

// deleteMachine deletes one of a cluster's workers and shrinks its pool by one, as
// `kubectl annotate machine <name> cluster.x-k8s.io/delete-machine=` and lowering the pool's
// replicas in the Cluster do: Cluster API then drains and deletes that machine before any other.
func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request, u user) {
	c, m, ok := s.clusterMachine(w, r, u)
	if !ok {
		return
	}
	pool := m.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel]
	i := slices.IndexFunc(c.Spec.Topology.Workers.MachineDeployments, func(md clusterv1.MachineDeploymentTopology) bool { return md.Name == pool })
	switch {
	case pool == "" || i < 0:
		writeError(w, http.StatusConflict, "Only a worker pool's machine can be deleted. Replace a control-plane node instead.")
		return
	case isAutoscaled(c, pool):
		writeError(w, http.StatusConflict, "Pool "+pool+" is autoscaled. Switch autoscaling off to delete a worker.")
		return
	case m.DeletionTimestamp != nil:
		writeError(w, http.StatusConflict, "The machine is already being deleted.")
		return
	}
	ctx := r.Context()
	mark := func(ctx context.Context, v any) error {
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{clusterv1.DeleteMachineAnnotation: v}}})
		if err != nil {
			return fmt.Errorf("marking the machine: %w", err)
		}
		return u.kube.Patch(ctx, m, client.RawPatch(types.MergePatchType, patch))
	}
	// Marked first: a pool that shrank before would delete whichever machine it picks.
	if err := mark(ctx, ""); err != nil {
		s.kubeError(w, err)
		return
	}
	before := c.DeepCopy()
	md := &c.Spec.Topology.Workers.MachineDeployments[i]
	md.Replicas = new(max(ptr.Deref(md.Replicas, 1)-1, 0))
	if err := u.kube.Patch(ctx, c, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if undoErr := mark(ctx, nil); undoErr != nil {
			s.Log.Error("unmarking a machine whose pool did not shrink", "machine", m.Namespace+"/"+m.Name, "error", undoErr)
		}
		s.kubeError(w, err)
		return
	}
	s.Log.Info("machine deleted", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "machine", m.Name, "pool", pool)
	w.WriteHeader(http.StatusNoContent)
}

// clusterMachine reads the cluster the request names, for a user who may change it, and the
// machine it names, which must be one of the cluster's.
func (s *Server) clusterMachine(w http.ResponseWriter, r *http.Request, u user) (*clusterv1.Cluster, *clusterv1.Machine, bool) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return nil, nil, false
	}
	m := &clusterv1.Machine{}
	err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("machine")}, m)
	if err != nil && !apierrors.IsNotFound(err) {
		s.kubeError(w, err)
		return nil, nil, false
	}
	if err != nil || m.Labels[clusterv1.ClusterNameLabel] != c.Name {
		writeError(w, http.StatusNotFound, "no such machine")
		return nil, nil, false
	}
	return c, m, true
}

// patchMachine applies patch to the machine the request names, which must be one of its cluster's.
func (s *Server) patchMachine(w http.ResponseWriter, r *http.Request, u user, patch []byte) {
	c, m, ok := s.clusterMachine(w, r, u)
	if !ok {
		return
	}
	if err := u.kube.Patch(r.Context(), m, client.RawPatch(types.MergePatchType, patch)); err != nil {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("machine changed", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "machine", m.Name, "patch", string(patch))
	w.WriteHeader(http.StatusNoContent)
}
