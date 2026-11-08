package kubehenge_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type schedulerHints struct {
	MemoryAdjustment *int64 `json:"memoryAdjustment"`
}

type clusterTemplateSpec struct {
	SchedulerHints schedulerHints `json:"schedulerHints"`
}

type clusterTemplateBody struct {
	Spec clusterTemplateSpec `json:"spec"`
}

type clusterTemplateOuter struct {
	Template clusterTemplateBody `json:"template"`
}

type clusterTemplate struct {
	Kind string               `json:"kind"`
	Spec clusterTemplateOuter `json:"spec"`
}

// TestEveryClusterTemplateTurnsOffCapmoxsMemoryReservation holds the classes to capmox's switch for
// its memory rule: left at its default it refuses every VM on a host whose guests' configured
// memory exceeds the host's.
func TestEveryClusterTemplateTurnsOffCapmoxsMemoryReservation(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	found := 0
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var obj clusterTemplate
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind != "ProxmoxClusterTemplate" {
			continue
		}
		found++
		if got := obj.Spec.Template.Spec.SchedulerHints.MemoryAdjustment; got == nil || *got != 0 {
			t.Errorf("a ProxmoxClusterTemplate has memoryAdjustment %v, want 0", got)
		}
	}
	const distributions = 4
	if found != distributions {
		t.Errorf("found %d ProxmoxClusterTemplates, want %d (kubeadm, RKE2, k3s and Talos)", found, distributions)
	}
}
