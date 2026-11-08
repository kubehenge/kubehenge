package kubehenge_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	fielderrors "k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// renderedClass is what Helm renders of a class's file: its variables, its patches and the
// templates the controller applies from the class's ConfigMap, by kind.
type renderedClass struct {
	variables map[string]*apiextensionsv1.JSONSchemaProps
	patches   []classPatch
	templates map[string]map[string]any
}

type variableClassSpec struct {
	Patches   []classPatch    `json:"patches"`
	Variables []classVariable `json:"variables"`
}

// asMap reads a rendered document's field as an object.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}
	return m
}

func renderClass(t *testing.T, file string) renderedClass {
	t.Helper()
	cmd := exec.CommandContext( //nolint:gosec // fixed arguments, only the file under the chart varies
		t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/"+file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	class := renderedClass{variables: map[string]*apiextensionsv1.JSONSchemaProps{}, templates: map[string]map[string]any{}}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var head struct {
			Kind string            `json:"kind"`
			Data map[string]string `json:"data"`
			Spec variableClassSpec `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatal(err)
		}
		switch head.Kind {
		case "ClusterClass":
			class.patches = head.Spec.Patches
			for i := range head.Spec.Variables {
				v := &head.Spec.Variables[i]
				class.variables[v.Name] = &v.Schema.OpenAPIV3Schema
			}
		case "ConfigMap":
			for tpl := range strings.SplitSeq(head.Data["templates.yaml"], "\n---\n") {
				var obj map[string]any
				if err := yaml.Unmarshal([]byte(tpl), &obj); err != nil {
					t.Fatal(err)
				}
				if kind, ok := obj["kind"].(string); ok {
					class.templates[kind] = obj
				}
			}
		}
	}
	return class
}

// patched is a bootstrap template as Cluster API leaves it for a pool whose variables are these:
// the class's patches that match it and are enabled, rendered and applied.
func (c renderedClass) patched(t *testing.T, kind string, variables map[string]any) map[string]any {
	t.Helper()
	doc, err := json.Marshal(c.templates[kind])
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range c.patches {
		if p.EnabledIf != "" && capiTemplate(t, p.EnabledIf, variables) != "true" {
			continue
		}
		for _, d := range p.Definitions {
			if d.Selector.Kind != kind {
				continue
			}
			for _, jp := range d.JSONPatches {
				value := jp.Value
				if jp.ValueFrom != nil {
					if err := yaml.Unmarshal([]byte(capiTemplate(t, jp.ValueFrom.Template, variables)), &value); err != nil {
						t.Fatalf("patch %s: %v", p.Name, err)
					}
				}
				op, err := json.Marshal([]map[string]any{{"op": jp.Op, "path": jp.Path, "value": value}})
				if err != nil {
					t.Fatal(err)
				}
				patch, err := jsonpatch.DecodePatch(op)
				if err != nil {
					t.Fatal(err)
				}
				if doc, err = patch.Apply(doc); err != nil {
					t.Fatalf("patch %s on %s: %v", p.Name, kind, err)
				}
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal(doc, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const poolNodes = `{"workerNodes": {
	"labels": [{"key": "team", "value": "a"}, {"key": "tier", "value": ""}],
	"taints": [{"key": "dedicated", "value": "db", "effect": "NoSchedule"}, {"key": "spot", "value": "", "effect": "PreferNoSchedule"}]
}}`

func variablesOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestKubeadmWorkersRegisterWithTheirPoolsLabelsAndTaints(t *testing.T) {
	t.Parallel()
	class := renderClass(t, "kubeadm.yaml")
	registration := func(vars string) map[string]any {
		spec := class.patched(t, "KubeadmConfigTemplate", variablesOf(t, vars))
		return asMap(t, field(t, spec, "spec", "template", "spec", "joinConfiguration", "nodeRegistration"))
	}
	got := registration(poolNodes)
	if args := asJSON(t, got["kubeletExtraArgs"]); !strings.Contains(args, `{"name":"node-labels","value":"team=a,tier="}`) {
		t.Errorf("kubelet arguments %s", args)
	}
	if got := asJSON(t, got["taints"]); got != `[{"effect":"NoSchedule","key":"dedicated","value":"db"},{"effect":"PreferNoSchedule","key":"spot","value":""}]` {
		t.Errorf("taints %s", got)
	}
	for _, vars := range []string{`{}`, `{"workerNodes": {}}`, `{"workerNodes": {"labels": [], "taints": []}}`} {
		bare := registration(vars)
		if strings.Contains(asJSON(t, bare["kubeletExtraArgs"]), "node-labels") || bare["taints"] != nil {
			t.Errorf("%s: labels or taints without any: %v", vars, bare)
		}
	}
	labelsOnly := registration(`{"workerNodes": {"labels": [{"key": "team", "value": "a"}]}}`)
	if labelsOnly["taints"] != nil || !strings.Contains(asJSON(t, labelsOnly["kubeletExtraArgs"]), `"team=a"`) {
		t.Errorf("labels only: %v", labelsOnly)
	}
}

func TestAgentWorkersRegisterWithTheirPoolsLabelsAndTaints(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, kind string }{{"rke2.yaml", "RKE2ConfigTemplate"}, {"k3s.yaml", "KThreesConfigTemplate"}} {
		class := renderClass(t, c.file)
		agent := func(vars string) map[string]any {
			spec := class.patched(t, c.kind, variablesOf(t, vars))
			return asMap(t, field(t, spec, "spec", "template", "spec", "agentConfig"))
		}
		got := agent(poolNodes)
		if v := asJSON(t, got["nodeLabels"]); v != `["team=a","tier="]` {
			t.Errorf("%s labels %s", c.file, v)
		}
		if v := asJSON(t, got["nodeTaints"]); v != `["dedicated=db:NoSchedule","spot=:PreferNoSchedule"]` {
			t.Errorf("%s taints %s", c.file, v)
		}
		for _, vars := range []string{`{}`, `{"workerNodes": {}}`} {
			if bare := agent(vars); bare["nodeLabels"] != nil || bare["nodeTaints"] != nil {
				t.Errorf("%s %s: labels or taints without any: %v", c.file, vars, bare)
			}
		}
	}
}

func TestTalosWorkersRegisterWithTheirPoolsLabelsAndTaints(t *testing.T) {
	t.Parallel()
	class := renderTalosClass(t)
	docs := talosConfig(t, machine.TypeWorker, class.strategicPatches(t, "TalosConfigTemplate", variablesOf(t, poolNodes)))
	node := docs["KubeNodeConfig"]
	if got := asJSON(t, node["labels"]); got != `{"team":"a","tier":""}` {
		t.Errorf("labels %s", got)
	}
	if got := asJSON(t, node["taints"]); got != `{"dedicated":"db:NoSchedule","spot":":PreferNoSchedule"}` {
		t.Errorf("taints %s", got)
	}
	bare := talosConfig(t, machine.TypeWorker, class.strategicPatches(t, "TalosConfigTemplate", variablesOf(t, `{"workerNodes": {}}`)))["KubeNodeConfig"]
	if bare["labels"] != nil || bare["taints"] != nil {
		t.Errorf("labels or taints without any: %v", bare)
	}
}

// validateVariable runs a class variable's schema and CEL rules as the API server does a Cluster's
// topology variable, with the value it had before for rules that compare.
func validateVariable(t *testing.T, schema *apiextensionsv1.JSONSchemaProps, value, old any) fielderrors.ErrorList {
	t.Helper()
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	validator, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	errs := validation.ValidateCustomResource(fielderrors.NewPath("value"), value, validator)
	celErrs, _ := cel.NewValidator(structural, true, celconfig.PerCallLimit).Validate(t.Context(), fielderrors.NewPath("value"), structural, value, old, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

func TestNodeLabelsAndTaintsAreValidatedByTheClass(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"} {
		schema, ok := renderClass(t, file).variables["workerNodes"]
		if !ok {
			t.Fatalf("%s has no workerNodes", file)
		}
		for name, c := range map[string]struct{ value, want string }{
			"fine":                  {`{"labels":[{"key":"team","value":"a"},{"key":"example.org/team","value":""},{"key":"node.kubernetes.io/pool","value":"x"},{"key":"kubernetes.io/os","value":"linux"}],"taints":[{"key":"dedicated","value":"db","effect":"NoExecute"},{"key":"k8s.io/x","effect":"NoSchedule"}]}`, ""},
			"reserved domain":       {`{"labels":[{"key":"kubernetes.io/role","value":"x"}]}`, "kubelet refuses to set labels in kubernetes.io and k8s.io"},
			"reserved subdomain":    {`{"labels":[{"key":"foo.k8s.io/x","value":"x"}]}`, "kubelet refuses to set labels in kubernetes.io and k8s.io"},
			"node role":             {`{"labels":[{"key":"node-role.kubernetes.io/db","value":""}]}`, "kubelet refuses to set labels in kubernetes.io and k8s.io"},
			"empty label key":       {`{"labels":[{"key":"","value":"x"}]}`, "key"},
			"label key syntax":      {`{"labels":[{"key":"-team","value":"x"}]}`, "key"},
			"label key twice":       {`{"labels":[{"key":"team","value":"a"},{"key":"team","value":"b"}]}`, "Each label key once"},
			"label name too long":   {`{"labels":[{"key":"` + strings.Repeat("a", 64) + `","value":""}]}`, "at most 63 characters"},
			"label value syntax":    {`{"labels":[{"key":"team","value":"a b"}]}`, "value"},
			"label value too long":  {`{"labels":[{"key":"team","value":"` + strings.Repeat("a", 64) + `"}]}`, "value"},
			"empty taint key":       {`{"taints":[{"key":"","effect":"NoSchedule"}]}`, "key"},
			"bad taint effect":      {`{"taints":[{"key":"dedicated","effect":"Evict"}]}`, "effect"},
			"no taint effect":       {`{"taints":[{"key":"dedicated"}]}`, "effect"},
			"taint key twice":       {`{"taints":[{"key":"d","effect":"NoSchedule"},{"key":"d","effect":"NoExecute"}]}`, "Each taint key once"},
			"taint name too long":   {`{"taints":[{"key":"` + strings.Repeat("a", 64) + `","effect":"NoSchedule"}]}`, "at most 63 characters"},
			"taint prefix too long": {`{"taints":[{"key":"` + strings.Repeat("a", 254) + `/x","effect":"NoSchedule"}]}`, "key"},
		} {
			var value any
			if err := json.Unmarshal([]byte(c.value), &value); err != nil {
				t.Fatal(err)
			}
			errs := validateVariable(t, schema, value, nil)
			switch {
			case c.want == "" && len(errs) > 0:
				t.Errorf("%s, %s: %v", file, name, errs)
			case c.want != "" && (len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), c.want)):
				t.Errorf("%s, %s: %v, want %q", file, name, errs, c.want)
			}
		}
	}
}

func TestCISMayBeSwitchedOnAndNeverOff(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"} {
		schema := renderClass(t, file).variables["cis"]
		for _, c := range []struct {
			old, value any
			want       string
		}{
			{false, true, ""},
			{false, false, ""},
			{true, true, ""},
			{nil, true, ""},
			{true, false, "CIS hardening cannot be switched off once it is on"},
		} {
			errs := validateVariable(t, schema, c.value, c.old)
			switch {
			case c.want == "" && len(errs) > 0:
				t.Errorf("%s: %v to %v: %v", file, c.old, c.value, errs)
			case c.want != "" && (len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), c.want)):
				t.Errorf("%s: %v to %v: %v, want %q", file, c.old, c.value, errs, c.want)
			}
		}
	}
}

func TestNodeSettingsReachOnlyTheWorkers(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, kind string }{{"kubeadm.yaml", "KubeadmControlPlaneTemplate"}, {"rke2.yaml", "RKE2ControlPlaneTemplate"}, {"k3s.yaml", "KThreesControlPlaneTemplate"}} {
		class := renderClass(t, c.file)
		after := asJSON(t, class.patched(t, c.kind, variablesOf(t, poolNodes)))
		if strings.Contains(after, "node-labels") || strings.Contains(after, "nodeLabels") || strings.Contains(after, "team=a") {
			t.Errorf("%s: the control plane takes the workers' labels: %s", c.file, after)
		}
	}
}
