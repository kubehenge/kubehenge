package kubehenge_test

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"
)

type resourceRule struct {
	APIVersions []string `json:"apiVersions"`
	Operations  []string `json:"operations"`
	Resources   []string `json:"resources"`
}

type matchConstraints struct {
	ResourceRules []resourceRule `json:"resourceRules"`
}

type policyExpression struct {
	Expression        string `json:"expression"`
	MessageExpression string `json:"messageExpression"`
}

type policySpec struct {
	MatchConstraints matchConstraints   `json:"matchConstraints"`
	MatchConditions  []policyExpression `json:"matchConditions"`
	Validations      []policyExpression `json:"validations"`
}

type admissionPolicy struct {
	Kind string     `json:"kind"`
	Spec policySpec `json:"spec"`
}

// controllerDocuments renders the chart's controller.yaml into its YAML documents.
func controllerDocuments(t *testing.T) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/controller.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	return strings.Split(string(out), "\n---")
}

// deletionPolicy renders the chart and finds its policy on deleting clusters.
func deletionPolicy(t *testing.T) admissionPolicy {
	t.Helper()
	for _, doc := range controllerDocuments(t) {
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

// deletedCluster is the cluster a delete request is for. Unset, it is in the clusters namespace and
// its class in kubehenge-system.
type deletedCluster struct {
	namespace, class, classNamespace string
	// noClassNamespace leaves classRef without a namespace, and noTopology the spec without topology.
	noClassNamespace, noTopology bool
	annotations                  map[string]any
	deleting                     bool
}

func (d *deletedCluster) ns() string {
	if d.namespace == "" {
		return "clusters"
	}
	return d.namespace
}

// object is the cluster as the policy reads it in oldObject.
func (d *deletedCluster) object() map[string]any {
	meta := map[string]any{"name": "web", "namespace": d.ns()}
	if d.annotations != nil {
		meta["annotations"] = d.annotations
	}
	if d.deleting {
		meta["deletionTimestamp"] = "2026-11-14T10:00:00Z"
	}
	if d.noTopology {
		return map[string]any{"metadata": meta, "spec": map[string]any{}}
	}
	ref := map[string]any{"name": d.class}
	switch {
	case d.noClassNamespace:
	case d.classNamespace != "":
		ref["namespace"] = d.classNamespace
	default:
		ref["namespace"] = "kubehenge-system"
	}
	return map[string]any{"metadata": meta, "spec": map[string]any{"topology": map[string]any{"classRef": ref}}}
}

// eval runs one of the policy's expressions against the cluster being deleted.
func eval(t *testing.T, expression string, d *deletedCluster) any {
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
	got, _, err := program.Eval(map[string]any{"oldObject": d.object(), "request": map[string]any{"namespace": d.ns()}})
	if err != nil {
		t.Fatalf("%s: %v", expression, err)
	}
	return got.Value()
}

// holds tells whether every expression is true for the cluster.
func holds(t *testing.T, expressions []policyExpression, d *deletedCluster) bool {
	t.Helper()
	for _, e := range expressions {
		got, ok := eval(t, e.Expression, d).(bool)
		if !ok {
			t.Fatalf("%s gives no bool", e.Expression)
		}
		if !got {
			return false
		}
	}
	return true
}

// A Kubehenge cluster is deleted only once it carries the annotation that lifts the guard, or while
// its deletion already runs. Clusters of other classes, those of a class named like Kubehenge's in
// another namespace included, are left alone.
func TestDeletingAKubehengeClusterNeedsTheAnnotation(t *testing.T) {
	t.Parallel()
	p := deletionPolicy(t)
	for name, tc := range map[string]struct {
		cluster        deletedCluster
		guarded, allow bool
	}{
		"bare":              {deletedCluster{class: "kubehenge-talos"}, true, false},
		"other annotations": {deletedCluster{class: "kubehenge-kubeadm", annotations: map[string]any{"a": "b"}}, true, false},
		"annotated":         {deletedCluster{class: "kubehenge-rke2", annotations: map[string]any{"kubehenge.dev/allow-deletion": "true"}}, true, true},
		"already deleting":  {deletedCluster{class: "kubehenge-k3s", deleting: true}, true, true},
		"another class":     {deletedCluster{class: "mine"}, false, false},
		"no topology":       {deletedCluster{noTopology: true}, false, false},
		"a class named like Kubehenge's in another namespace": {
			deletedCluster{namespace: "team-a", class: "kubehenge-mine", classNamespace: "team-a"}, false, false,
		},
		"a class named like Kubehenge's in the cluster's own namespace": {
			deletedCluster{namespace: "team-a", class: "kubehenge-mine", noClassNamespace: true}, false, false,
		},
		"no class namespace, the cluster beside Kubehenge's classes": {
			deletedCluster{namespace: "kubehenge-system", class: "kubehenge-rke2", noClassNamespace: true}, true, false,
		},
	} {
		if got := holds(t, p.Spec.MatchConditions, &tc.cluster); got != tc.guarded {
			t.Errorf("%s: guarded %v, want %v", name, got, tc.guarded)
			continue
		}
		if tc.guarded && holds(t, p.Spec.Validations, &tc.cluster) != tc.allow {
			t.Errorf("%s: deletion allowed %v, want %v", name, !tc.allow, tc.allow)
		}
	}
	msg, ok := eval(t, p.Spec.Validations[0].MessageExpression, &deletedCluster{class: "kubehenge-talos"}).(string)
	if !ok || !strings.Contains(msg, "kubehenge.dev/allow-deletion") || !strings.Contains(msg, "web") {
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
	repository := os.DirFS("../..")
	var files []string
	for _, pattern := range []string{"test/e2e/*.sh", "docs/*.md", "website/.vitepress/theme/*.vue", "website/.vitepress/theme/*.js"} {
		matched, err := fs.Glob(repository, pattern)
		if err != nil || len(matched) == 0 {
			t.Fatalf("%s: %v, %d files", pattern, err, len(matched))
		}
		files = append(files, matched...)
	}
	for _, file := range files {
		text, err := fs.ReadFile(repository, file)
		if err != nil {
			t.Fatal(err)
		}
		script := path.Ext(file) == ".sh"
		previous, number := "", 0
		for line := range strings.SplitSeq(string(text), "\n") {
			number++
			// A script's own deletes are checked by running it, so only what it prints is a hint. A
			// doc's commands are its code blocks' lines. Its tables name add-on namespaces, which
			// hold no clusters.
			printed := strings.Contains(line, "printf ")
			switch path.Ext(file) {
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
