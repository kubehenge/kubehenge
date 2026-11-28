package kubehenge_test

import (
	"maps"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// namePrefixAnnotation is what Kubehenge's capmox build names a machine's VM and host by.
const namePrefixAnnotation = "kubehenge.dev/vm-name-prefix"

// classNaming is the naming templates of a class's control plane and worker pools.
type classNaming struct {
	Spec namingSpec `json:"spec"`
}

type namingSpec struct {
	ControlPlane named        `json:"controlPlane"`
	Workers      namedWorkers `json:"workers"`
}

type namedWorkers struct {
	MachineDeployments []named `json:"machineDeployments"`
}

type named struct {
	Naming naming `json:"naming"`
}

type naming struct {
	Template string `json:"template"`
}

func namingOf(t *testing.T, file string) classNaming {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system", //nolint:gosec // the test's own arguments
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/"+file)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var c struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
			t.Fatal(err)
		}
		if c.Kind == "ClusterClass" {
			var n classNaming
			if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("no ClusterClass")
	return classNaming{}
}

// firewallAnnotations are a machine template's annotations without the name prefix, which every
// class writes beside the firewall's.
func firewallAnnotations(t *testing.T, annotations any) map[string]any {
	t.Helper()
	m := maps.Clone(asMap(t, annotations))
	delete(m, namePrefixAnnotation)
	return m
}

// Every class names its control plane <cluster>-cp and each pool <cluster>-<pool>, with no random
// part, so that the Machines each makes, in a new cluster and in every replacement, are
// <cluster>-cp-<random> and <cluster>-<pool>-<random>-<random>. Each machine template carries the
// prefix capmox names the VM and its host with: the cluster's name, or the nodeNamePrefix variable.
func TestEveryClassNamesMachinesAfterTheCluster(t *testing.T) {
	t.Parallel()
	for _, file := range everyClass {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			naming := namingOf(t, file)
			if got := naming.Spec.ControlPlane.Naming.Template; got != "{{ .cluster.name }}-cp" {
				t.Errorf("control-plane naming = %q", got)
			}
			if len(naming.Spec.Workers.MachineDeployments) != 1 {
				t.Fatalf("%d machine deployment classes, want one", len(naming.Spec.Workers.MachineDeployments))
			}
			if got := naming.Spec.Workers.MachineDeployments[0].Naming.Template; got != "{{ .cluster.name }}-{{ .machineDeployment.topologyName }}" {
				t.Errorf("worker naming = %q", got)
			}
			class := renderClass(t, file)
			prefix := func(extra string) any {
				vars := variablesOf(t, `{"builtin": {"cluster": {"name": "dev", "namespace": "n", "network": {"pods": ["10.244.0.0/16"]}}},
					"controlPlaneEndpoint": "10.0.0.5", "network": `+ipv4Net+extra+`}`)
				return asMap(t, field(t, class.patched(t, "ProxmoxMachineTemplate", vars), "spec", "template", "metadata", "annotations"))[namePrefixAnnotation]
			}
			for extra, want := range map[string]string{
				"": "dev", `, "nodeNamePrefix": ""`: "dev", `, "nodeNamePrefix": "kubehenge-dev"`: "kubehenge-dev",
				`, "nodeNamePrefix": "kubehenge-dev", "firewall": {"enabled": false}`: "kubehenge-dev",
			} {
				if got := prefix(extra); got != want {
					t.Errorf("prefix with%s = %v, want %s", extra, got, want)
				}
			}
			schema := class.variables["nodeNamePrefix"]
			if schema == nil || schema.Type != "string" || schema.MaxLength == nil || *schema.MaxLength != 40 || schema.Default == nil || string(schema.Default.Raw) != `""` {
				t.Fatalf("nodeNamePrefix = %+v, want a string of at most 40 characters, empty by default", schema)
			}
			pattern := regexp.MustCompile(schema.Pattern)
			for value, ok := range map[string]bool{"": true, "kubehenge-dev": true, "a1": true, "Dev": false, "-dev": false, "dev-": false, "1dev": false, "dev.x": false} {
				if pattern.MatchString(value) != ok {
					t.Errorf("the pattern takes %q: %v, want %v", value, !ok, ok)
				}
			}
		})
	}
}
