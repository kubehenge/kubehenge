package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func customised(value string) *clusterv1.Cluster {
	c := cluster("default", "one")
	c.Spec.Topology.Variables = []clusterv1.ClusterVariable{{Name: provisioner.NodeCustomisationVariable, Value: apiextensionsv1.JSON{Raw: []byte(value)}}}
	return c
}

// The plan says that a referenced Secret or ConfigMap also replaces the nodes, and only then.
func TestPlanSaysWhyNodesRollOnReferencedContent(t *testing.T) {
	t.Parallel()
	changes := []plannedChange{{Setting: provisioner.NodeCustomisationVariable, Effect: effectAll}}
	rolls := func(roles ...string) string { return provisioner.NodeCustomisationRolls(roles) }
	if got := planWarnings(customised(`{"auditWebhook":{"secret":"siem"}}`), changes); !slices.Contains(got, rolls("controlPlane")) {
		t.Errorf("a Secret: %v", got)
	}
	if got := planWarnings(customised(`{"auditPolicy":{"configMap":"p"}}`), changes); !slices.Contains(got, rolls("controlPlane")) {
		t.Errorf("a ConfigMap: %v", got)
	}
	workers := `{"files":[{"path":"/etc/x","roles":["worker"],"secret":{"name":"s","key":"k"}}]}`
	if got := planWarnings(customised(workers), changes); !slices.Contains(got, rolls("worker")) || slices.Contains(got, rolls("controlPlane")) {
		t.Errorf("a worker file: %v", got)
	}
	if got := planWarnings(customised(`{"files":[{"path":"/etc/x","content":"x"}]}`), changes); slices.ContainsFunc(got, func(w string) bool { return strings.Contains(w, "also replaced") }) {
		t.Errorf("inline files: %v", got)
	}
	one := customised(`{"auditWebhook":{"secret":"siem"}}`)
	one.Spec.Topology.ControlPlane.Replicas = new(int32(1))
	cp := []plannedChange{{Setting: provisioner.NodeCustomisationVariable, Effect: effectControlPlane}}
	if got := planWarnings(one, cp); len(got) != 2 {
		t.Errorf("one control plane: %v", got)
	}
}

// Health shows a reference that is missing.
func TestHealthShowsAMissingNodeCustomisationReference(t *testing.T) {
	t.Parallel()
	c := customised(`{}`)
	c.Status.Conditions = []metav1.Condition{{Type: provisioner.NodeCustomisationCondition, Status: metav1.ConditionTrue, Reason: provisioner.NodeCustomisationMissing, Message: "Secret siem is not in namespace default"}}
	var a attention
	watchAttention(c, &a)
	if len(a) != 1 || a[0].What != "Node customisation misses a reference" || a[0].Message != "Secret siem is not in namespace default" {
		t.Errorf("attention %+v", a)
	}
}

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

// The server compares its client with the one it watches, which panics for a type that cannot be
// compared.
func TestDemoClientCanBeCompared(t *testing.T) {
	t.Parallel()
	s, _ := newDemo(t)
	if s.currentKube() != s.currentKube() {
		t.Error("the demo's client differs from itself")
	}
}

// The demo keeps what its cluster's node customisation names, or saving any change to it is refused.
func TestDemoHasWhatItsNodeCustomisationNames(t *testing.T) {
	t.Parallel()
	s, _ := newDemo(t)
	for _, key := range []string{"payments-ca", "payments-siem"} {
		if err := s.Kube.Get(t.Context(), client.ObjectKey{Namespace: demoNamespace, Name: key}, &corev1.Secret{}); err != nil {
			t.Errorf("Secret %s: %v", key, err)
		}
	}
	if err := s.Kube.Get(t.Context(), client.ObjectKey{Namespace: demoNamespace, Name: "payments-audit-policy"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("ConfigMap: %v", err)
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
