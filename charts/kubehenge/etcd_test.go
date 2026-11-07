package kubehenge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"
)

// classDocument is a rendered ClusterClass, with its variables as the API server stores them.
type classDocument struct {
	Kind     string        `json:"kind"`
	Metadata classMetadata `json:"metadata"`
	Spec     classContent  `json:"spec"`
}

type classMetadata struct {
	Name string `json:"name"`
}

type classContent struct {
	Variables []classVariable `json:"variables"`
	Patches   []classPatch    `json:"patches"`
}

type classVariable struct {
	Name   string         `json:"name"`
	Schema variableSchema `json:"schema"`
}

type variableSchema struct {
	OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
}

type etcdTalosConfig struct {
	Cluster talosClusterConfig `json:"cluster"`
}

type talosClusterConfig struct {
	Etcd talosEtcdConfig `json:"etcd"`
}

type talosEtcdConfig struct {
	ExtraArgs map[string]string `json:"extraArgs"`
}

// renderedClasses is every ClusterClass the chart renders, by name. Rendering the whole chart keeps
// the helm arguments constant.
var renderedClasses = sync.OnceValues(func() (map[string]classDocument, error) {
	cmd := exec.CommandContext(context.Background(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("helm template: %w: %s", err, stderr.String())
	}
	classes := map[string]classDocument{}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var d classDocument
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			continue
		}
		if d.Kind == "ClusterClass" {
			classes[d.Metadata.Name] = d
		}
	}
	return classes, nil
})

func renderClass(t *testing.T, file string) *classDocument {
	t.Helper()
	classes, err := renderedClasses()
	if err != nil {
		t.Fatal(err)
	}
	name := "kubehenge-" + strings.TrimSuffix(file, ".yaml")
	d, ok := classes[name]
	if !ok {
		t.Fatalf("%s renders no ClusterClass", file)
	}
	return &d
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	return m
}

func (d *classDocument) patch(t *testing.T, name string) classPatch {
	t.Helper()
	for _, p := range d.Spec.Patches {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no patch %s", name)
	return classPatch{}
}

type kubeadmArg struct{ Name, Value string }

// kubeadmLocalEtcd is the local member of kubeadm's etcd configuration.
type kubeadmLocalEtcd struct {
	ExtraArgs []kubeadmArg `json:"extraArgs"`
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
		// The patch adds etcd whole: the CRD refuses an empty etcd.local in the template to add to.
		if jp.Path != "/spec/template/spec/kubeadmConfigSpec/clusterConfiguration/etcd" {
			t.Fatalf("path %s", jp.Path)
		}
		var etcd struct {
			Local kubeadmLocalEtcd `json:"local"`
		}
		if err := yaml.Unmarshal([]byte(rendered), &etcd); err != nil {
			t.Fatalf("%v:\n%s", err, rendered)
		}
		out := make([]string, 0, len(etcd.Local.ExtraArgs))
		for _, a := range etcd.Local.ExtraArgs {
			out = append(out, a.Name+"="+a.Value)
		}
		return out
	case "talos.yaml":
		var doc string
		if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
			t.Fatalf("%v:\n%s", err, rendered)
		}
		var cfg etcdTalosConfig
		if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(cfg.Cluster.Etcd.ExtraArgs))
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
			props := asMap(t, v.Schema.OpenAPIV3Schema["properties"])
			quota := asMap(t, props["quotaGiB"])
			if quota["minimum"] != float64(2) || quota["maximum"] != float64(8) || quota["default"] != float64(8) || quota["type"] != "integer" {
				t.Errorf("%s: quotaGiB = %v", file, quota)
			}
			rules, ok := v.Schema.OpenAPIV3Schema["x-kubernetes-validations"].([]any)
			if !ok || len(rules) != 1 {
				t.Errorf("%s: the quota cannot be lowered: %v", file, rules)
			}
		}
		if !found {
			t.Errorf("%s has no etcd variable", file)
		}
	}
}
