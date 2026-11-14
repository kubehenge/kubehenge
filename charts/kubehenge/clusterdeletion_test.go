package kubehenge_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	meta, _ := old["metadata"].(map[string]any)
	got, _, err := program.Eval(map[string]any{"oldObject": old, "request": map[string]any{"namespace": meta["namespace"]}})
	if err != nil {
		t.Fatalf("%s: %v", expression, err)
	}
	return got.Value()
}

// A Kubehenge cluster is deleted only once it carries the annotation that lifts the guard, or while
// its deletion already runs. Clusters of other classes, those of a class named like Kubehenge's in
// another namespace included, are left alone.
func TestDeletingAKubehengeClusterNeedsTheAnnotation(t *testing.T) {
	t.Parallel()
	p := deletionPolicy(t)
	in := func(c map[string]any, namespace, classNamespace string) map[string]any {
		c["metadata"].(map[string]any)["namespace"] = namespace
		ref := c["spec"].(map[string]any)["topology"].(map[string]any)["classRef"].(map[string]any)
		if classNamespace == "" {
			delete(ref, "namespace")
		} else {
			ref["namespace"] = classNamespace
		}
		return c
	}
	cluster := func(class string, annotations map[string]any, deleting bool) map[string]any {
		ref := map[string]any{"name": class, "namespace": "kubehenge-system"}
		meta := map[string]any{"name": "web", "namespace": "clusters"}
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
		"a class named like Kubehenge's in another namespace":           {in(cluster("kubehenge-mine", nil, false), "team-a", "team-a"), false, false},
		"a class named like Kubehenge's in the cluster's own namespace": {in(cluster("kubehenge-mine", nil, false), "team-a", ""), false, false},
		"no class namespace, the cluster beside Kubehenge's classes":    {in(cluster("kubehenge-rke2", nil, false), "kubehenge-system", ""), true, false},
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

// kubectlDelete is a kubectl command that deletes clusters, or a namespace that may hold them.
var kubectlDelete = regexp.MustCompile(`kubectl\b.*\bdelete (namespace|cluster|clusters)\b`)

// Every delete the e2e failure hints, the docs and the website print is one the guard lets through:
// the annotate step comes first, on the line itself or the one before. A bare one is refused, or
// leaves its namespace Terminating.
func TestEveryPrintedClusterDeleteAnnotatesFirst(t *testing.T) {
	t.Parallel()
	var files []string
	for _, pattern := range []string{"../../test/e2e/*.sh", "../../docs/*.md", "../../website/.vitepress/theme/*.vue", "../../website/.vitepress/theme/*.js"} {
		matched, err := filepath.Glob(pattern)
		if err != nil || len(matched) == 0 {
			t.Fatalf("%s: %v, %d files", pattern, err, len(matched))
		}
		files = append(files, matched...)
	}
	for _, file := range files {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		script := strings.HasSuffix(file, ".sh")
		previous, number := "", 0
		for line := range strings.SplitSeq(string(text), "\n") {
			number++
			// A script's own deletes are checked by running it, so only what it prints is a hint. A
			// doc's commands are its code blocks' lines. Its tables name add-on namespaces, which
			// hold no clusters.
			printed := strings.Contains(line, "printf ")
			switch filepath.Ext(file) {
			case ".md":
				printed = strings.HasPrefix(strings.TrimSpace(line), "kubectl ")
			case ".vue", ".js":
				printed = true
			}
			if at := kubectlDelete.FindStringIndex(line); printed && at != nil {
				if !strings.Contains(line[:at[1]], "allow-deletion") && (script || !strings.Contains(previous, "allow-deletion")) {
					t.Errorf("%s:%d prints a delete without annotating first: %s", file, number, strings.TrimSpace(line))
				}
			}
			if strings.TrimSpace(line) != "" {
				previous = line
			}
		}
	}
}
