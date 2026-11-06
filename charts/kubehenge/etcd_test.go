package kubehenge_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// classDocument is a rendered ClusterClass, with its variables as the API server stores them.
type classDocument struct {
	Kind string `json:"kind"`
	Spec struct {
		Variables []struct {
			Name   string `json:"name"`
			Schema struct {
				OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"variables"`
		Patches []classPatch `json:"patches"`
	} `json:"spec"`
}

func renderClass(t *testing.T, file string) classDocument {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/"+file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var d classDocument
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatal(err)
		}
		if d.Kind == "ClusterClass" {
			return d
		}
	}
	t.Fatalf("%s renders no ClusterClass", file)
	return classDocument{}
}

func (d classDocument) patch(t *testing.T, name string) classPatch {
	t.Helper()
	for _, p := range d.Spec.Patches {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no patch %s", name)
	return classPatch{}
}

// etcdArguments are the etcd arguments a distribution's patch renders for a cluster's variables, as
// name=value, in order.
func etcdArguments(t *testing.T, file, patch, variables string) []string {
	t.Helper()
	var vars map[string]any
	if err := json.Unmarshal([]byte(variables), &vars); err != nil {
		t.Fatal(err)
	}
	p := renderClass(t, file).patch(t, patch)
	if len(p.Definitions) != 1 || len(p.Definitions[0].JSONPatches) != 1 {
		t.Fatalf("patch %s: %+v", patch, p.Definitions)
	}
	jp := p.Definitions[0].JSONPatches[0]
	rendered := capiTemplate(t, jp.ValueFrom.Template, vars)
	switch file {
	case "kubeadm.yaml":
		if jp.Path != "/spec/template/spec/kubeadmConfigSpec/clusterConfiguration/etcd/local/extraArgs" {
			t.Fatalf("path %s", jp.Path)
		}
		var args []struct{ Name, Value string }
		if err := yaml.Unmarshal([]byte(rendered), &args); err != nil {
			t.Fatalf("%v:\n%s", err, rendered)
		}
		var out []string
		for _, a := range args {
			out = append(out, a.Name+"="+a.Value)
		}
		return out
	case "talos.yaml":
		var doc string
		if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
			t.Fatalf("%v:\n%s", err, rendered)
		}
		var cfg struct {
			Cluster struct {
				Etcd struct {
					ExtraArgs map[string]string `json:"extraArgs"`
				} `json:"etcd"`
			} `json:"cluster"`
		}
		if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Fatal(err)
		}
		var out []string
		for k, v := range cfg.Cluster.Etcd.ExtraArgs {
			out = append(out, k+"="+v)
		}
		slices.Sort(out)
		return out
	}
	var content struct {
		Args []string `json:"etcd-arg+"`
	}
	var file2 struct {
		Content string `json:"content"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &file2); err != nil {
		t.Fatalf("%v:\n%s", err, rendered)
	}
	if err := yaml.Unmarshal([]byte(file2.Content), &content); err != nil {
		t.Fatal(err)
	}
	return content.Args
}

func TestEtcdArguments(t *testing.T) {
	t.Parallel()
	const periodic, hour = "auto-compaction-mode=periodic", "auto-compaction-retention=1h"
	for _, c := range []struct {
		file, patch, variables, quota string
		sorted                        bool
	}{
		{"kubeadm.yaml", "kubeadm-etcd", `{}`, "8589934592", false},
		{"kubeadm.yaml", "kubeadm-etcd", `{"etcd":{"quotaGiB":4}}`, "4294967296", false},
		{"rke2.yaml", "rke2-etcd", `{}`, "8589934592", false},
		{"rke2.yaml", "rke2-etcd", `{"etcd":{"quotaGiB":3}}`, "3221225472", false},
		{"k3s.yaml", "k3s-etcd", `{}`, "8589934592", false},
		{"k3s.yaml", "k3s-etcd", `{"etcd":{"quotaGiB":2}}`, "2147483648", false},
		{"talos.yaml", "talos-etcd", `{}`, "8589934592", true},
		{"talos.yaml", "talos-etcd", `{"etcd":{"quotaGiB":6}}`, "6442450944", true},
	} {
		want := []string{"quota-backend-bytes=" + c.quota, periodic, hour}
		if c.sorted {
			want = []string{periodic, hour, "quota-backend-bytes=" + c.quota}
		}
		if got := etcdArguments(t, c.file, c.patch, c.variables); !slices.Equal(got, want) {
			t.Errorf("%s %s: arguments %v, want %v", c.file, c.variables, got, want)
		}
	}
}

func TestEtcdVariableBounds(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"} {
		found := false
		for _, v := range renderClass(t, file).Spec.Variables {
			if v.Name != "etcd" {
				continue
			}
			found = true
			props, _ := v.Schema.OpenAPIV3Schema["properties"].(map[string]any)
			quota, _ := props["quotaGiB"].(map[string]any)
			if quota["minimum"] != float64(2) || quota["maximum"] != float64(8) || quota["default"] != float64(8) || quota["type"] != "integer" {
				t.Errorf("%s: quotaGiB = %v", file, quota)
			}
			if rules, _ := v.Schema.OpenAPIV3Schema["x-kubernetes-validations"].([]any); len(rules) != 1 {
				t.Errorf("%s: the quota cannot be lowered: %v", file, rules)
			}
		}
		if !found {
			t.Errorf("%s has no etcd variable", file)
		}
	}
}
