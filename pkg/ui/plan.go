package ui

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	writeJSON(w, http.StatusOK, planView{Cluster: s.view(ch.c, ch.access.path, ch.access.held, u.namespaces), Changes: changes})
}

// plannedOf is what each setting of a change does to the cluster's machines, by its class.
func plannedOf(ctx context.Context, u user, ch *changed) ([]plannedChange, error) {
	class := &clusterv1.ClusterClass{}
	ref := ch.before.Spec.Topology.ClassRef
	if err := u.kube.Get(ctx, client.ObjectKey{Namespace: classNamespace(ref.Namespace, ch.before.Namespace), Name: ref.Name}, class); err != nil {
		return nil, fmt.Errorf("reading the cluster's class %s: %w", ref.Name, err)
	}
	return planned(ch, variableReach(class)), nil
}

// classNamespace is where a cluster's class lives: the reference's namespace, else the cluster's.
func classNamespace(ref, cluster string) string {
	if ref != "" {
		return ref
	}
	return cluster
}

// planned are the settings a change sets to something new, in the order the request names them.
func planned(ch *changed, reach map[string]machineReach) []plannedChange {
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
		out = append(out, plannedChange{Setting: name, Effect: reach[name].effect()})
	}
	return out
}

// machineReach is which machines' templates a variable feeds through the class's patches.
type machineReach struct {
	controlPlane, workers bool
}

func (m machineReach) effect() string {
	switch {
	case m.controlPlane && m.workers:
		return effectAll
	case m.controlPlane:
		return effectControlPlane
	case m.workers:
		return effectWorkers
	default:
		return effectNone
	}
}

// templateName finds the names a patch's Go template reads, as .name at the start of a pipeline's
// term; a field of a value it ranges over or takes with "with" reads the same, which errs on the
// side of saying a change replaces machines.
var templateName = regexp.MustCompile(`(?:^|[\s({|$=,])\.([A-Za-z_][A-Za-z0-9_]*)`)

// variableReach reads, from a class's patches, which variables feed the control plane's or the
// workers' templates: a new value there makes Cluster API replace those machines.
func variableReach(class *clusterv1.ClusterClass) map[string]machineReach {
	out := map[string]machineReach{}
	for i := range class.Spec.Patches {
		reach, found := patchReach(&class.Spec.Patches[i])
		for _, n := range found {
			was := out[n]
			out[n] = machineReach{controlPlane: was.controlPlane || reach.controlPlane, workers: was.workers || reach.workers}
		}
	}
	delete(out, "builtin")
	return out
}

// patchReach is which machines' templates a patch changes, and the variables it reads.
func patchReach(p *clusterv1.ClusterClassPatch) (machineReach, []string) {
	var reach machineReach
	found := templateNames(p.EnabledIf)
	for _, d := range p.Definitions {
		m := d.Selector.MatchResources
		reach.controlPlane = reach.controlPlane || m.ControlPlane != nil && *m.ControlPlane
		reach.workers = reach.workers || m.MachineDeploymentClass != nil || m.MachinePoolClass != nil
		for _, jp := range d.JSONPatches {
			if jp.ValueFrom == nil {
				continue
			}
			if v := jp.ValueFrom.Variable; v != "" {
				root, _, _ := strings.Cut(v, ".")
				found = append(found, root)
			}
			found = append(found, templateNames(jp.ValueFrom.Template)...)
		}
	}
	return reach, found
}

func templateNames(template string) []string {
	found := templateName.FindAllStringSubmatch(template, -1)
	out := make([]string, 0, len(found))
	for _, m := range found {
		out = append(out, m[1])
	}
	return out
}
