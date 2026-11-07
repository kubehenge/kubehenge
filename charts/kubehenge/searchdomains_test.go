package kubehenge_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	fieldpath "k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// classVariable is a variable of a ClusterClass as Helm renders it.
type classVariable struct {
	Name   string         `json:"name"`
	Schema variableSchema `json:"schema"`
}

type variableSchema struct {
	OpenAPIV3Schema apiextensionsv1.JSONSchemaProps `json:"openAPIV3Schema"`
}

type variablesDocument struct {
	Kind string        `json:"kind"`
	Spec variablesSpec `json:"spec"`
}

type variablesSpec struct {
	Variables []classVariable `json:"variables"`
}

// distributionSearchDomains is the patch that gives a kubeadm, RKE2 or k3s class's nodes the
// network variable's search domains, as the class renders.
func distributionSearchDomains(t *testing.T, file, name string) classPatch {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system", //nolint:gosec // the chart's own file
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/"+file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var class renderedDocument
		if err := yaml.Unmarshal([]byte(doc), &class); err != nil {
			t.Fatal(err)
		}
		for _, p := range class.Spec.Patches {
			if class.Kind == "ClusterClass" && p.Name == name {
				return p
			}
		}
	}
	t.Fatalf("%s has no patch %s", file, name)
	return classPatch{}
}

func TestSearchDomainsReachEveryNode(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		file, patch, controlPlane, worker, commands string
	}{
		{"kubeadm.yaml", "kubeadm-search-domains", "/spec/template/spec/kubeadmConfigSpec", "/spec/template/spec", "preKubeadmCommands"},
		{"rke2.yaml", "rke2-search-domains", "/spec/template/spec", "/spec/template/spec", "preRKE2Commands"},
		{"k3s.yaml", "k3s-search-domains", "/spec/template/spec/kthreesConfigSpec", "/spec/template/spec", "preK3sCommands"},
	} {
		t.Run(c.patch, func(t *testing.T) {
			t.Parallel()
			patch := distributionSearchDomains(t, c.file, c.patch)
			network := map[string]any{"searchDomains": []any{"corp.example", "example.org"}}
			if got := capiTemplate(t, patch.EnabledIf, map[string]any{"network": network}); got != "true" {
				t.Errorf("enabledIf with domains = %q", got)
			}
			for _, without := range []map[string]any{
				{"network": map[string]any{"gateway": "192.0.2.1"}},
				{"network": map[string]any{"searchDomains": []any{}}},
				{},
			} {
				if got := capiTemplate(t, patch.EnabledIf, without); got != "" {
					t.Errorf("enabledIf without domains (%v) = %q", without, got)
				}
			}
			if len(patch.Definitions) != 2 {
				t.Fatalf("%d definitions, want the control planes and the workers", len(patch.Definitions))
			}
			for i, want := range []string{c.controlPlane, c.worker} {
				var file, command string
				for _, jp := range patch.Definitions[i].JSONPatches {
					switch jp.Path {
					case want + "/files/-":
						file = capiTemplate(t, jp.ValueFrom.Template, map[string]any{"network": network})
					case want + "/" + c.commands + "/-":
						text, ok := jp.Value.(string)
						if !ok {
							t.Fatalf("command %v", jp.Value)
						}
						command = text
					}
				}
				var written struct {
					Path        string `json:"path"`
					Owner       string `json:"owner"`
					Permissions string `json:"permissions"`
					Content     string `json:"content"`
				}
				if err := yaml.UnmarshalStrict([]byte(file), &written); err != nil {
					t.Fatalf("%s: %v:\n%s", patch.Definitions[i].Selector.Kind, err, file)
				}
				if written.Path != command || written.Permissions != "0700" || written.Owner != "root:root" {
					t.Errorf("%s: file %s (%s) run as %q", patch.Definitions[i].Selector.Kind, written.Path, written.Permissions, command)
				}
				if !strings.Contains(written.Content, `ipv4.dns-search "corp.example example.org"`) {
					t.Errorf("%s: script = %s", patch.Definitions[i].Selector.Kind, written.Content)
				}
			}
		})
	}
}

// networkValidator validates a value against the class's network variable as the API server does.
func networkValidator(t *testing.T) validation.SchemaValidator { //nolint:ireturn // the API server's own validator type
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/kubeadm.yaml")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var class variablesDocument
		if err := yaml.Unmarshal([]byte(doc), &class); err != nil {
			t.Fatal(err)
		}
		for i := range class.Spec.Variables {
			v := &class.Spec.Variables[i]
			if class.Kind != "ClusterClass" || v.Name != "network" {
				continue
			}
			var internal apiextensions.JSONSchemaProps
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&v.Schema.OpenAPIV3Schema, &internal, nil); err != nil {
				t.Fatal(err)
			}
			validator, _, err := validation.NewSchemaValidator(&internal)
			if err != nil {
				t.Fatal(err)
			}
			return validator
		}
	}
	t.Fatal("no network variable")
	return nil
}

func TestSearchDomainsAreDNSNames(t *testing.T) {
	t.Parallel()
	validator := networkValidator(t)
	network := func(domains string) map[string]any {
		var value map[string]any
		raw := `{"bridge":"vmbr0","addresses":["192.0.2.10"],"prefix":24,"gateway":"192.0.2.1","dnsServers":["192.0.2.1"]` + domains + `}`
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	long := strings.Repeat("a", 64)
	for _, c := range []struct {
		name    string
		domains string
		valid   bool
	}{
		{"unset", ``, true},
		{"one", `,"searchDomains":["corp.example"]`, true},
		{"single label", `,"searchDomains":["corp"]`, true},
		{"six", `,"searchDomains":["a.example","b.example","c.example","d.example","e.example","f.example"]`, true},
		{"mixed case and digits", `,"searchDomains":["Site-1.Corp.example"]`, true},
		{"seven", `,"searchDomains":["a.example","b.example","c.example","d.example","e.example","f.example","g.example"]`, false},
		{"a space", `,"searchDomains":["corp.example other.example"]`, false},
		{"a quote", `,"searchDomains":["corp\".example"]`, false},
		{"a shell word", `,"searchDomains":["$(reboot).example"]`, false},
		{"a leading hyphen", `,"searchDomains":["-corp.example"]`, false},
		{"an empty label", `,"searchDomains":["corp..example"]`, false},
		{"empty", `,"searchDomains":[""]`, false},
		{"a trailing dot", `,"searchDomains":["corp.example."]`, false},
		{"a label of 64", `,"searchDomains":["` + long + `.example"]`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			errs := validation.ValidateCustomResource(fieldpath.NewPath("network"), network(c.domains), validator)
			if (len(errs) == 0) != c.valid {
				t.Errorf("valid = %v, errors %v", len(errs) == 0, errs)
			}
		})
	}
}

func TestTalosSearchDomains(t *testing.T) {
	t.Parallel()
	class := renderTalosClass(t)
	variables := map[string]any{
		"controlPlaneEndpoint": "192.0.2.10",
		"cni":                  "cilium",
		"network":              map[string]any{"searchDomains": []any{"corp.example", "example.org"}},
	}
	for kind, role := range map[string]machine.Type{"TalosControlPlaneTemplate": machine.TypeControlPlane, "TalosConfigTemplate": machine.TypeWorker} {
		docs := talosConfig(t, role, class.strategicPatches(t, kind, variables))
		if got := asJSON(t, field(t, docs["ResolverConfig"], "searchDomains", "domains")); got != `["corp.example","example.org"]` {
			t.Errorf("%s: search domains = %s", kind, got)
		}
		if _, ok := docs["ResolverConfig"]["nameservers"]; ok {
			t.Errorf("%s: the resolver document sets nameservers, which the platform's would lose", kind)
		}
		bare := talosConfig(t, role, class.strategicPatches(t, kind, map[string]any{"controlPlaneEndpoint": "192.0.2.10", "cni": "cilium"}))
		if _, ok := bare["ResolverConfig"]["searchDomains"]; ok {
			t.Errorf("%s: a resolver document without search domains", kind)
		}
	}
}
