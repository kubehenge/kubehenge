package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
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

// cordonMachine keeps new pods off a machine's node, as `kubectl cordon` does.
func (s *Server) cordonMachine(w http.ResponseWriter, r *http.Request, u user) {
	s.cordon(w, r, u, true)
}

// uncordonMachine lets pods onto a machine's node again, as `kubectl uncordon` does.
func (s *Server) uncordonMachine(w http.ResponseWriter, r *http.Request, u user) {
	s.cordon(w, r, u, false)
}

// cordon sets whether pods may be scheduled on a machine's node, as the cluster's nodes identity,
// which may change nothing else of a node.
func (s *Server) cordon(w http.ResponseWriter, r *http.Request, u user, cordoned bool) {
	c, m, ok := s.clusterMachine(w, r, u, privChange)
	if !ok {
		return
	}
	node := m.Status.NodeRef.Name
	if node == "" {
		writeError(w, http.StatusConflict, "The machine has no node yet.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthDeadline)
	defer cancel()
	wl, ranOut, err := s.reach(ctx, u.kube, c, provisioner.IdentityNodes)
	switch {
	case err != nil:
		s.kubeError(w, r, err)
		return
	case wl == nil && ranOut.IsZero():
		writeError(w, http.StatusConflict, "Node access to this cluster is not set up yet.")
		return
	case wl == nil:
		writeError(w, http.StatusConflict, "Access to the cluster expired. Waiting for it to answer.")
		return
	}
	patch := fmt.Appendf(nil, `{"spec":{"unschedulable":%t}}`, cordoned)
	if _, err := wl.Kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusConflict, "Node "+node+" is not in the cluster.")
			return
		}
		s.Log.Info("a cluster's node request failed", "cluster", c.Namespace+"/"+c.Name, "node", node, "error", err)
		writeError(w, http.StatusBadGateway, "the cluster did not answer")
		return
	}
	s.Log.Info("node cordon set", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "node", node, "cordoned", cordoned)
	w.WriteHeader(http.StatusNoContent)
}

// rebootMachine reboots a machine's VM in Proxmox VE as the user, who needs VM.PowerMgmt on it.
// Proxmox VE shuts the guest down and starts it again. The request does not wait for that.
func (s *Server) rebootMachine(w http.ResponseWriter, r *http.Request, u user) {
	c, m, ok := s.clusterMachine(w, r, u, privPower)
	if !ok {
		return
	}
	pool, err := s.poolPath(c.Namespace, c.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such cluster")
		return
	}
	vm, found := s.clusterVMs(r, u, c, pool)[m.Spec.InfrastructureRef.Name]
	if !found || vm.VMID == 0 || vm.Node == "" {
		writeError(w, http.StatusConflict, "The machine has no VM in the cluster's pool.")
		return
	}
	if err := s.Accounts.RebootVM(r.Context(), u.ticket, vm.Node, vm.VMID); err != nil {
		s.proxmoxError(w, r, err)
		return
	}
	s.Log.Info("machine rebooted", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "machine", m.Name, "vmid", vm.VMID)
	w.WriteHeader(http.StatusAccepted)
}

// deleteMachine deletes one of a cluster's workers and shrinks its pool by one, as
// `kubectl annotate machine <name> cluster.x-k8s.io/delete-machine=` and lowering the pool's
// replicas in the Cluster do: Cluster API then drains and deletes that machine before any other.
func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request, u user) {
	c, m, ok := s.clusterMachine(w, r, u, privChange)
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
		return annotate(ctx, u.kube, m, map[string]any{clusterv1.DeleteMachineAnnotation: v})
	}
	// Marked first: a pool that shrank before would delete whichever machine it picks.
	if err := mark(ctx, ""); err != nil {
		s.kubeError(w, r, err)
		return
	}
	before := c.DeepCopy()
	md := &c.Spec.Topology.Workers.MachineDeployments[i]
	md.Replicas = new(max(ptr.Deref(md.Replicas, 1)-1, 0))
	if err := u.kube.Patch(ctx, c, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if undoErr := mark(ctx, nil); undoErr != nil {
			s.Log.Error("unmarking a machine whose pool did not shrink", "machine", m.Namespace+"/"+m.Name, "error", undoErr)
		}
		s.kubeError(w, r, err)
		return
	}
	s.Log.Info("machine deleted", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "machine", m.Name, "pool", pool)
	w.WriteHeader(http.StatusNoContent)
}

// clusterMachine reads the cluster the request names, for a user who holds privs on it, and the
// machine it names, which must be one of the cluster's.
func (s *Server) clusterMachine(w http.ResponseWriter, r *http.Request, u user, privs ...string) (*clusterv1.Cluster, *clusterv1.Machine, bool) {
	c, _, ok := s.cluster(w, r, u, privs...)
	if !ok {
		return nil, nil, false
	}
	m := &clusterv1.Machine{}
	err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("machine")}, m)
	if err != nil && !apierrors.IsNotFound(err) {
		s.kubeError(w, r, err)
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
	c, m, ok := s.clusterMachine(w, r, u, privChange)
	if !ok {
		return
	}
	if err := u.kube.Patch(r.Context(), m, client.RawPatch(types.MergePatchType, patch)); err != nil {
		s.kubeError(w, r, err)
		return
	}
	s.Log.Info("machine changed", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "machine", m.Name, "patch", string(patch))
	w.WriteHeader(http.StatusNoContent)
}

// annotate sets each of an object's annotations to its value, or removes it where the value is nil.
func annotate(ctx context.Context, kube client.Client, obj client.Object, values map[string]any) error {
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": values}})
	if err != nil {
		return fmt.Errorf("encoding the annotations: %w", err)
	}
	return kube.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw)) //nolint:wrapcheck // kubeError answers as the API server did
}
