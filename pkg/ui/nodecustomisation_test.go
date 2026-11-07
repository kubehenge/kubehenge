package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// The chart's classes reach every node with nodeCustomisation, so a change to it replaces them
// all, which the plan says.
func TestNodeCustomisationReplacesEveryNode(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", "../../charts/kubehenge", "--namespace", "kubehenge-system",
		"--values", "../provisioner/testdata/integration-values.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	seen := 0
	for doc := range strings.SplitSeq(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n---\n") {
		var class clusterv1.ClusterClass
		if err := yaml.Unmarshal([]byte(doc), &class); err != nil || class.Kind != "ClusterClass" {
			continue
		}
		seen++
		reach := provisioner.VariableReach(&class)[provisioner.NodeCustomisationVariable]
		if effectOf(reach) != effectAll {
			t.Errorf("%s: a change of nodeCustomisation replaces %q, want all", class.Name, effectOf(reach))
		}
	}
	if seen != len(provisioner.VersionFormats) {
		t.Errorf("%d classes read", seen)
	}
	if !changeableVariables[provisioner.NodeCustomisationVariable] {
		t.Error("nodeCustomisation cannot change after creation")
	}
}

// The demo's API server refuses a protected path as the webhook does.
func TestDemoRefusesWhatTheWebhookRefuses(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	plan := func(value string) *httptest.ResponseRecorder {
		return b.do("POST", "/api/clusters/"+demoNamespace+"/payments/plan", `{"variables":{"nodeCustomisation":`+value+`}}`)
	}
	rec := plan(`{"files":[{"path":"/etc/kubernetes/pki/ca.crt","content":"x"}]}`)
	if rec.Code < http.StatusBadRequest || !strings.Contains(rec.Body.String(), "/etc/kubernetes/pki/ca.crt is a path Kubehenge keeps") {
		t.Errorf("a protected path: %d %s", rec.Code, rec.Body)
	}
	if rec := plan(`{"files":[{"path":"/etc/audit/x","content":"x"}]}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"all"`) {
		t.Errorf("a free path: %d %s", rec.Code, rec.Body)
	}
}
