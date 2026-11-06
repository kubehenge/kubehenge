package kubehenge_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type deletionDocument struct {
	Kind     string            `json:"kind"`
	Metadata deletionName      `json:"metadata"`
	Spec     deletionClassSpec `json:"spec"`
}

type deletionName struct {
	Name string `json:"name"`
}

type deletionClassSpec struct {
	ControlPlane deletionMachines `json:"controlPlane"`
	Workers      deletionWorkers  `json:"workers"`
}

type deletionMachines struct {
	Class    string         `json:"class"`
	Deletion map[string]int `json:"deletion"`
}

type deletionWorkers struct {
	MachineDeployments []deletionMachines `json:"machineDeployments"`
}

// Cluster API waits for ever on a disruption budget or a volume that will not detach unless the
// class bounds it, so every class bounds the wait for its control planes and each worker class.
func TestEveryClassBoundsTheWaitForARemovedMachinesNode(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	want := map[string]int{"nodeDrainTimeoutSeconds": 900, "nodeVolumeDetachTimeoutSeconds": 600, "nodeDeletionTimeoutSeconds": 60}
	same := func(got map[string]int) bool {
		if len(got) != len(want) {
			return false
		}
		for k, v := range want {
			if got[k] != v {
				return false
			}
		}
		return true
	}
	classes := 0
	for doc := range strings.SplitSeq(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n---\n") {
		var d deletionDocument
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatal(err)
		}
		if d.Kind != "ClusterClass" {
			continue
		}
		classes++
		if !same(d.Spec.ControlPlane.Deletion) {
			t.Errorf("%s: control plane deletion = %v, want %v", d.Metadata.Name, d.Spec.ControlPlane.Deletion, want)
		}
		if len(d.Spec.Workers.MachineDeployments) == 0 {
			t.Errorf("%s: no worker classes", d.Metadata.Name)
		}
		for _, md := range d.Spec.Workers.MachineDeployments {
			if !same(md.Deletion) {
				t.Errorf("%s: worker class %s deletion = %v, want %v", d.Metadata.Name, md.Class, md.Deletion, want)
			}
		}
	}
	const distributions = 4
	if classes != distributions {
		t.Errorf("the chart renders %d ClusterClasses, want %d", classes, distributions)
	}
}
