package ui

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

var updateDemoClasses = flag.Bool("update-demo-classes", false, "write demo-classes.json from the chart")

// chartDocument is what the demo keeps of a document the chart renders.
type chartDocument struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   chartMetadata `json:"metadata"`
	Spec       chartSpec     `json:"spec"`
}

type chartMetadata struct {
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations"`
}

type chartSpec struct {
	Variables    []any             `json:"variables"`
	ControlPlane chartMachineClass `json:"controlPlane"`
	Workers      chartWorkerClass  `json:"workers"`
}

type chartWorkerClass struct {
	MachineDeployments []chartMachineClass `json:"machineDeployments"`
}

// chartMachineClass is the part of a control plane or a worker class that the wizard shows: the
// waits a removed machine's node is given.
type chartMachineClass struct {
	Class    string         `json:"class,omitempty"`
	Deletion map[string]int `json:"deletion"`
}

// checkHostOutageGraceSchema holds every ClusterClass to the variable's default of 30 minutes and
// its rule that 0 or at least 5 is allowed, since the wizard and the webhook read the same numbers.
func checkHostOutageGraceSchema(t *testing.T, class *chartDocument) {
	t.Helper()
	for _, v := range class.Spec.Variables {
		variable, isObject := v.(map[string]any)
		if !isObject || variable["name"] != provisioner.HostOutageGraceVariable {
			continue
		}
		schema, err := json.Marshal(variable["schema"])
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"default":30`, `"minimum":0`, `"maximum":10080`, `"rule":"self == 0 || self `, `= 5"`} {
			if !strings.Contains(string(schema), want) {
				t.Errorf("class %s: %s lacks %s: %s", class.Metadata.Name, provisioner.HostOutageGraceVariable, want, schema)
			}
		}
		return
	}
	t.Errorf("class %s has no variable %s", class.Metadata.Name, provisioner.HostOutageGraceVariable)
}

// reachPatches are patches that reach the machines the class's own patches reach for each variable
// and hold nothing else, so that the demo's plan says which machines a change replaces.
func reachPatches(class *clusterv1.ClusterClass) []any {
	reach := provisioner.VariableReach(class)
	var patches []any
	names := make([]string, 0, len(class.Spec.Variables))
	for i := range class.Spec.Variables {
		names = append(names, class.Spec.Variables[i].Name)
	}
	slices.Sort(names)
	for _, name := range names {
		r, ok := reach[name]
		if !ok || (!r.ControlPlane && !r.Workers) {
			continue
		}
		match := map[string]any{}
		if r.ControlPlane {
			match["controlPlane"] = true
		}
		if r.Workers {
			match["machineDeploymentClass"] = map[string]any{"names": []string{"worker"}}
		}
		patches = append(patches, map[string]any{
			"name": "reach-" + name,
			"definitions": []any{map[string]any{
				"selector": map[string]any{"apiVersion": "v1", "kind": "Reach", "matchResources": match},
				"jsonPatches": []any{map[string]any{
					"op": "add", "path": "/spec", "valueFrom": map[string]any{"template": "{{ ." + name + " }}"},
				}},
			}},
		})
	}
	return patches
}

// The demo's ClusterClasses are the chart's, cut to what the interface reads, so the wizard it
// shows is the real one.
func TestDemoClassesMatchTheChart(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", "../../charts/kubehenge", "--namespace", "kubehenge-system",
		"--values", "../provisioner/testdata/integration-values.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	var classes []map[string]any
	for doc := range strings.SplitSeq(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n---\n") {
		var obj chartDocument
		if yamlErr := yaml.Unmarshal([]byte(doc), &obj); yamlErr != nil {
			t.Fatal(yamlErr)
		}
		if obj.Kind != "ClusterClass" {
			continue
		}
		checkHostOutageGraceSchema(t, &obj)
		var full clusterv1.ClusterClass
		if yamlErr := yaml.Unmarshal([]byte(doc), &full); yamlErr != nil {
			t.Fatal(yamlErr)
		}
		classes = append(classes, map[string]any{
			"apiVersion": obj.APIVersion,
			"kind":       obj.Kind,
			"metadata":   map[string]any{"name": obj.Metadata.Name, "namespace": "kubehenge-system", "annotations": obj.Metadata.Annotations},
			"spec": map[string]any{
				"variables":    obj.Spec.Variables,
				"controlPlane": map[string]any{"deletion": obj.Spec.ControlPlane.Deletion},
				"workers":      map[string]any{"machineDeployments": obj.Spec.Workers.MachineDeployments},
				"patches":      reachPatches(&full),
			},
		})
	}
	if len(classes) != len(provisioner.VersionFormats) {
		t.Fatalf("the chart renders %d ClusterClasses, want %d", len(classes), len(provisioner.VersionFormats))
	}
	want, err := json.MarshalIndent(classes, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *updateDemoClasses {
		if writeErr := os.WriteFile("demo-classes.json", want, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		return
	}
	got, err := os.ReadFile("demo-classes.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatal("demo-classes.json differs from the chart. Write it again: go test ./pkg/ui -run TestDemoClassesMatchTheChart -update-demo-classes")
	}
}
