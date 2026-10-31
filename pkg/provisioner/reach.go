package provisioner

import (
	"regexp"
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// MachineReach is which machines' templates a variable feeds through a class's patches.
type MachineReach struct {
	ControlPlane, Workers bool
}

// templateName finds the names a patch's Go template reads, as .name at the start of a pipeline's
// term. A field of a value it ranges over or takes with "with" reads the same, which errs on the
// side of saying a change replaces machines.
var templateName = regexp.MustCompile(`(?:^|[\s({|$=,])\.([A-Za-z_][A-Za-z0-9_]*)`)

// VariableReach reads, from a class's patches, which variables feed the control plane's or the
// workers' templates: a new value there makes Cluster API replace those machines.
func VariableReach(class *clusterv1.ClusterClass) map[string]MachineReach {
	out := map[string]MachineReach{}
	for i := range class.Spec.Patches {
		reach, found := patchReach(&class.Spec.Patches[i])
		for _, n := range found {
			was := out[n]
			out[n] = MachineReach{ControlPlane: was.ControlPlane || reach.ControlPlane, Workers: was.Workers || reach.Workers}
		}
	}
	delete(out, "builtin")
	return out
}

// patchReach is which machines' templates a patch changes, and the variables it reads.
func patchReach(p *clusterv1.ClusterClassPatch) (MachineReach, []string) {
	var reach MachineReach
	found := patchVariables(p.EnabledIf)
	for _, d := range p.Definitions {
		m := d.Selector.MatchResources
		reach.ControlPlane = reach.ControlPlane || m.ControlPlane != nil && *m.ControlPlane
		reach.Workers = reach.Workers || m.MachineDeploymentClass != nil || m.MachinePoolClass != nil
		for _, jp := range d.JSONPatches {
			if jp.ValueFrom == nil {
				continue
			}
			if v := jp.ValueFrom.Variable; v != "" {
				root, _, _ := strings.Cut(v, ".")
				found = append(found, root)
			}
			found = append(found, patchVariables(jp.ValueFrom.Template)...)
		}
	}
	return reach, found
}

// patchVariables are the names a patch's Go template reads.
func patchVariables(template string) []string {
	found := templateName.FindAllStringSubmatch(template, -1)
	out := make([]string, 0, len(found))
	for _, m := range found {
		out = append(out, m[1])
	}
	return out
}

// SingleControlPlaneRoll is the warning given before a change replaces a cluster's only control
// plane, in the interface's plan and as the admission webhook's warning to kubectl.
const SingleControlPlaneRoll = "Replaces the only control plane. The API stops if the old or new node fails mid-roll."

// rollsOnlyControlPlane tells whether a change replaces the only control plane of a cluster that
// keeps one: a new version, or a new value of a variable its class feeds into the control plane.
func rollsOnlyControlPlane(old, c *clusterv1.Cluster, class *clusterv1.ClusterClass) bool {
	if old == nil {
		return false
	}
	if r := c.Spec.Topology.ControlPlane.Replicas; r == nil || *r != 1 {
		return false
	}
	if old.Spec.Topology.Version != c.Spec.Topology.Version {
		return true
	}
	reach := VariableReach(class)
	return slices.ContainsFunc(c.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool {
		return reach[v.Name].ControlPlane && variableChanged(old, c, v.Name)
	}) || slices.ContainsFunc(old.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool {
		return reach[v.Name].ControlPlane && variableChanged(old, c, v.Name)
	})
}
