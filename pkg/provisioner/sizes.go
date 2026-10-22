package provisioner

import (
	"encoding/json"
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// ControlPlaneSizesAnnotation on a ClusterClass is its distribution's server sizing guide: the least
// vCPUs and memory its control planes need from a number of workers on, in rising order.
const ControlPlaneSizesAnnotation = "kubehenge.dev/control-plane-sizes"

type controlPlaneSize struct {
	Workers   int32 `json:"workers"`
	Cores     int   `json:"cores"`
	MemoryMiB int   `json:"memoryMiB"`
}

// covers reports whether nodes of size s are at least need.
func (s controlPlaneSize) covers(need controlPlaneSize) bool {
	return s.Cores >= need.Cores && s.MemoryMiB >= need.MemoryMiB
}

// machineSize is the part of a machine variable that sizes its nodes.
type machineSize struct {
	Cores     *int `json:"cores,omitempty"`
	MemoryMiB *int `json:"memoryMiB,omitempty"`
	DiskGiB   *int `json:"diskGiB,omitempty"`
}

// checkControlPlaneSize refuses a cluster whose control planes are smaller than its class's sizing
// guide gives for its workers. Updating a cluster that was already short is refused only when the
// update makes it shorter, so that it can still be changed in other ways once a newer class asks for
// more.
func checkControlPlaneSize(old, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	need, have, err := controlPlaneFit(cluster, class)
	if err != nil || have.covers(need) {
		return err
	}
	if old != nil {
		oldNeed, oldHave, err := controlPlaneFit(old, class)
		if err == nil && !oldHave.covers(oldNeed) && oldNeed.covers(need) && have.covers(oldHave) {
			return nil
		}
	}
	if have.Workers == 0 {
		return fmt.Errorf("the control planes of a cluster without workers run its workloads too and need at least %d vCPUs and %d MiB of memory, and have %d vCPUs and %d MiB",
			need.Cores, need.MemoryMiB, have.Cores, have.MemoryMiB)
	}
	return fmt.Errorf("the control planes of a cluster with %d workers need at least %d vCPUs and %d MiB of memory, and have %d vCPUs and %d MiB",
		have.Workers, need.Cores, need.MemoryMiB, have.Cores, have.MemoryMiB)
}

// controlPlaneFit is what the class's sizing guide asks of the cluster's control planes, and what
// they have, with the cluster's number of workers. A class without a guide asks nothing. The guides
// size servers whose workloads run on workers; without workers the control planes run them too
// (TaintReconciler), so they need the class's least worker on top.
func controlPlaneFit(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) (controlPlaneSize, controlPlaneSize, error) {
	var need, have controlPlaneSize
	topology := cluster.Spec.Topology
	for i := range topology.Workers.MachineDeployments {
		// An autoscaled pool may grow to its most, which the control planes have to serve.
		have.Workers += workerRange(&topology.Workers.MachineDeployments[i]).most
	}
	guide, ok := class.Annotations[ControlPlaneSizesAnnotation]
	if !ok {
		return need, have, nil
	}
	var sizes []controlPlaneSize
	if err := json.Unmarshal([]byte(guide), &sizes); err != nil {
		return need, have, fmt.Errorf("class %s, annotation %s: %w", class.Name, ControlPlaneSizesAnnotation, err)
	}
	for _, s := range sizes {
		if s.Workers <= have.Workers {
			need = s
		}
	}
	if have.Workers == 0 {
		worker := classMinimumSize(class, "workerMachine")
		need.Cores += worker.Cores
		need.MemoryMiB += worker.MemoryMiB
	}
	var err error
	have.Cores, have.MemoryMiB, err = nodeSize(cluster, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides)
	return need, have, err
}

// nodeSize is the vCPUs and memory a machine variable gives its nodes: from a machine deployment's
// or the control plane's override, else the cluster's value, else the class's default.
func nodeSize(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass, name string, overrides []clusterv1.ClusterVariable) (int, int, error) {
	set, err := nodeShape(cluster, class, name, overrides)
	if err != nil {
		return 0, 0, err
	}
	return *set.Cores, *set.MemoryMiB, nil
}

// nodeShape is what nodeSize reads, with the boot disk's size where the variable or the class gives
// one. Its vCPUs and memory are always set.
func nodeShape(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass, name string, overrides []clusterv1.ClusterVariable) (machineSize, error) {
	var set machineSize
	for _, vars := range [][]clusterv1.ClusterVariable{overrides, cluster.Spec.Topology.Variables} {
		v := variableNamed(vars, name)
		if v == nil {
			continue
		}
		if err := json.Unmarshal(v.Value.Raw, &set); err != nil {
			return set, fmt.Errorf("variable %s: %w", name, err)
		}
		break
	}
	if set.Cores == nil || set.MemoryMiB == nil || set.DiskGiB == nil {
		defaults, err := classDefaultSize(class, name)
		if err != nil {
			return set, err
		}
		if set.Cores == nil {
			set.Cores = defaults.Cores
		}
		if set.MemoryMiB == nil {
			set.MemoryMiB = defaults.MemoryMiB
		}
		if set.DiskGiB == nil {
			set.DiskGiB = defaults.DiskGiB
		}
	}
	if set.Cores == nil || set.MemoryMiB == nil {
		return set, fmt.Errorf("class %s gives %s no default vCPUs or memory", class.Name, name)
	}
	return set, nil
}

func variableNamed(vars []clusterv1.ClusterVariable, name string) *clusterv1.ClusterVariable {
	for i := range vars {
		if vars[i].Name == name {
			return &vars[i]
		}
	}
	return nil
}

// classMinimumSize is the least vCPUs and memory a class's machine variable lets a node have, zero
// where its schema sets no minimum.
func classMinimumSize(class *clusterv1.ClusterClass, name string) controlPlaneSize {
	var size controlPlaneSize
	for i := range class.Spec.Variables {
		v := &class.Spec.Variables[i]
		if v.Name != name {
			continue
		}
		for field, value := range map[string]*int{"cores": &size.Cores, "memoryMiB": &size.MemoryMiB} {
			if m := v.Schema.OpenAPIV3Schema.Properties[field].Minimum; m != nil {
				*value = int(*m)
			}
		}
	}
	return size
}

// classDefaultSize is the vCPUs and memory a class's machine variable defaults to: each field's own
// default, else the variable's.
func classDefaultSize(class *clusterv1.ClusterClass, name string) (machineSize, error) {
	var size machineSize
	for i := range class.Spec.Variables {
		v := &class.Spec.Variables[i]
		if v.Name != name {
			continue
		}
		schema := v.Schema.OpenAPIV3Schema
		if schema.Default != nil {
			if err := json.Unmarshal(schema.Default.Raw, &size); err != nil {
				return size, fmt.Errorf("class %s, variable %s: default: %w", class.Name, name, err)
			}
		}
		for field, value := range map[string]**int{"cores": &size.Cores, "memoryMiB": &size.MemoryMiB, "diskGiB": &size.DiskGiB} {
			d := schema.Properties[field].Default
			if d == nil {
				continue
			}
			var n int
			if err := json.Unmarshal(d.Raw, &n); err != nil {
				return size, fmt.Errorf("class %s, variable %s: default %s: %w", class.Name, name, field, err)
			}
			*value = &n
		}
	}
	return size, nil
}
