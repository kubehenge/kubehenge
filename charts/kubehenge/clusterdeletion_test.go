package kubehenge_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"
)

type admissionPolicy struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		MatchConstraints struct {
			ResourceRules []struct {
				Operations []string `json:"operations"`
				Resources  []string `json:"resources"`
			} `json:"resourceRules"`
		} `json:"matchConstraints"`
		MatchConditions []struct {
			Expression string `json:"expression"`
		} `json:"matchConditions"`
		Validations []struct {
			Expression        string `json:"expression"`
			MessageExpression string `json:"messageExpression"`
		} `json:"validations"`
	} `json:"spec"`
}

// deletionPolicy renders the chart and finds its policy on deleting clusters.
func deletionPolicy(t *testing.T) admissionPolicy {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/controller.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	for doc := range strings.SplitSeq(string(out), "\n---") {
		var p admissionPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatal(err)
		}
		if p.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		for _, r := range p.Spec.MatchConstraints.ResourceRules {
			if strings.Join(r.Operations, ",") == "DELETE" && strings.Join(r.Resources, ",") == "clusters" {
				return p
			}
		}
	}
	t.Fatal("the chart has no admission policy on deleting clusters")
	return admissionPolicy{}
}

// eval runs one of the policy's expressions against the cluster being deleted.
func eval(t *testing.T, expression string, old map[string]any) any {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatalf("%s: %v", expression, issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := program.Eval(map[string]any{"oldObject": old, "request": map[string]any{}})
	if err != nil {
		t.Fatalf("%s: %v", expression, err)
	}
	return got.Value()
}

// A Kubehenge cluster is deleted only once it carries the annotation that lifts the guard, or while
// its deletion already runs. Clusters of other classes are left alone.
func TestDeletingAKubehengeClusterNeedsTheAnnotation(t *testing.T) {
	t.Parallel()
	p := deletionPolicy(t)
	cluster := func(class string, annotations map[string]any, deleting bool) map[string]any {
		ref := map[string]any{"name": class}
		meta := map[string]any{"name": "web"}
		if annotations != nil {
			meta["annotations"] = annotations
		}
		if deleting {
			meta["deletionTimestamp"] = "2026-11-14T10:00:00Z"
		}
		return map[string]any{"metadata": meta, "spec": map[string]any{"topology": map[string]any{"classRef": ref}}}
	}
	matches := func(c map[string]any) bool {
		for _, m := range p.Spec.MatchConditions {
			if eval(t, m.Expression, c) != true {
				return false
			}
		}
		return true
	}
	allowed := func(c map[string]any) bool {
		for _, v := range p.Spec.Validations {
			if eval(t, v.Expression, c) != true {
				return false
			}
		}
		return true
	}
	for name, tc := range map[string]struct {
		cluster        map[string]any
		guarded, allow bool
	}{
		"bare":              {cluster("kubehenge-talos", nil, false), true, false},
		"other annotations": {cluster("kubehenge-kubeadm", map[string]any{"a": "b"}, false), true, false},
		"annotated":         {cluster("kubehenge-rke2", map[string]any{"kubehenge.dev/allow-deletion": "true"}, false), true, true},
		"already deleting":  {cluster("kubehenge-k3s", nil, true), true, true},
		"another class":     {cluster("mine", nil, false), false, false},
		"no topology":       {map[string]any{"metadata": map[string]any{"name": "web"}, "spec": map[string]any{}}, false, false},
	} {
		if got := matches(tc.cluster); got != tc.guarded {
			t.Errorf("%s: guarded %v, want %v", name, got, tc.guarded)
			continue
		}
		if tc.guarded && allowed(tc.cluster) != tc.allow {
			t.Errorf("%s: deletion allowed %v, want %v", name, !tc.allow, tc.allow)
		}
	}
	msg := eval(t, p.Spec.Validations[0].MessageExpression, cluster("kubehenge-talos", nil, false))
	if s, _ := msg.(string); !strings.Contains(s, "kubehenge.dev/allow-deletion") || !strings.Contains(s, "web") {
		t.Errorf("message %q names neither the cluster nor the annotation", msg)
	}
}
