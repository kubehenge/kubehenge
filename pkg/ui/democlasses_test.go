package ui

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"

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
	Variables []any `json:"variables"`
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
		classes = append(classes, map[string]any{
			"apiVersion": obj.APIVersion,
			"kind":       obj.Kind,
			"metadata":   map[string]any{"name": obj.Metadata.Name, "namespace": "kubehenge-system", "annotations": obj.Metadata.Annotations},
			"spec":       map[string]any{"variables": obj.Spec.Variables},
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
