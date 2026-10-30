package ui

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// What a change does to the cluster's machines.
const (
	// effectNone replaces no machine: an add-on or a setting Kubehenge's controller reads.
	effectNone = "none"
	// effectScales adds or removes machines and leaves the others as they are.
	effectScales = "scales"
	// effectControlPlane and effectWorkers replace those machines one at a time; effectAll both.
	effectControlPlane = "control-plane"
	effectWorkers      = "workers"
	effectAll          = "all"
)

// plannedChange is one setting a change sets anew, and what that does to the machines.
type plannedChange struct {
	// Setting is a variable's name, or version, controlPlaneReplicas, workerPools or autoscaling.
	Setting string `json:"setting"`
	Effect  string `json:"effect"`
}

// planView is a change as it would land: the cluster as it would be, and what each setting does.
type planView struct {
	Cluster clusterView     `json:"cluster"`
	Changes []plannedChange `json:"changes"`
	// Warnings are what to know before applying the change, such as a roll of the only control
	// plane; kubectl gets the same from the admission webhook.
	Warnings []string `json:"warnings,omitempty"`
}

// planWarnings are the warnings of a change to a cluster as it would be after it.
func planWarnings(after *clusterv1.Cluster, changes []plannedChange) []string {
	if r := after.Spec.Topology.ControlPlane.Replicas; r == nil || *r != 1 {
		return nil
	}
	if slices.ContainsFunc(changes, func(c plannedChange) bool { return c.Effect == effectControlPlane || c.Effect == effectAll }) {
		return []string{provisioner.SingleControlPlaneRoll}
	}
	return nil
}

// planCluster checks a change as changeCluster would make it, the API server's admission included,
// without making it, and says what each of its settings does to the machines.
func (s *Server) planCluster(w http.ResponseWriter, r *http.Request, u user) {
	ch, ok := s.prepareChange(w, r, u)
	if !ok {
		return
	}
	if err := u.kube.Update(r.Context(), ch.c, client.DryRunAll); err != nil {
		s.kubeError(w, r, err)
		return
	}
	changes, err := plannedOf(r.Context(), u, ch)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, planView{
		Cluster: s.view(ch.c, ch.access.path, ch.access.held, u.namespaces), Changes: changes, Warnings: planWarnings(ch.c, changes),
	})
}

// plannedOf is what each setting of a change does to the cluster's machines, by its class.
func plannedOf(ctx context.Context, u user, ch *changed) ([]plannedChange, error) {
	class := &clusterv1.ClusterClass{}
	ref := ch.before.Spec.Topology.ClassRef
	if err := u.kube.Get(ctx, client.ObjectKey{Namespace: classNamespace(ref.Namespace, ch.before.Namespace), Name: ref.Name}, class); err != nil {
		return nil, fmt.Errorf("reading the cluster's class %s: %w", ref.Name, err)
	}
	return planned(ch, provisioner.VariableReach(class)), nil
}

// classNamespace is where a cluster's class lives: the reference's namespace, else the cluster's.
func classNamespace(ref, cluster string) string {
	if ref != "" {
		return ref
	}
	return cluster
}

// planned are the settings a change sets to something new, in the order the request names them.
func planned(ch *changed, reach map[string]provisioner.MachineReach) []plannedChange {
	out := []plannedChange{}
	before := ch.before.Spec.Topology
	if v := ch.req.Version; v != nil && *v != before.Version {
		out = append(out, plannedChange{Setting: "version", Effect: effectAll})
	}
	if n := ch.req.ControlPlaneReplicas; n != nil && (before.ControlPlane.Replicas == nil || *n != *before.ControlPlane.Replicas) {
		out = append(out, plannedChange{Setting: "controlPlaneReplicas", Effect: effectScales})
	}
	if len(ch.req.WorkerPools) > 0 {
		out = append(out, plannedChange{Setting: "workerPools", Effect: effectScales})
	}
	if len(ch.req.Autoscaling) > 0 {
		out = append(out, plannedChange{Setting: "autoscaling", Effect: effectNone})
	}
	for _, name := range slices.Sorted(maps.Keys(ch.req.Variables)) {
		if sameJSON(variableValue(ch.before, name), ch.req.Variables[name]) {
			continue
		}
		out = append(out, plannedChange{Setting: name, Effect: effectOf(reach[name])})
	}
	return out
}

func effectOf(m provisioner.MachineReach) string {
	switch {
	case m.ControlPlane && m.Workers:
		return effectAll
	case m.ControlPlane:
		return effectControlPlane
	case m.Workers:
		return effectWorkers
	default:
		return effectNone
	}
}
