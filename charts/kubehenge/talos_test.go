package kubehenge_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/stdpatches"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"sigs.k8s.io/yaml"
)

// talosClass is what Helm renders of the Talos ClusterClass: the class and the templates the
// controller applies from its ConfigMap, by kind.
type talosClass struct {
	patches   []classPatch
	templates map[string]map[string]any
}

type classPatch struct {
	Name        string            `json:"name"`
	EnabledIf   string            `json:"enabledIf"`
	Definitions []patchDefinition `json:"definitions"`
}

type patchDefinition struct {
	Selector    patchSelector `json:"selector"`
	JSONPatches []jsonPatch   `json:"jsonPatches"`
}

type patchSelector struct {
	Kind string `json:"kind"`
}

type jsonPatch struct {
	Op        string          `json:"op"`
	Path      string          `json:"path"`
	Value     any             `json:"value"`
	ValueFrom *patchValueFrom `json:"valueFrom"`
}

type patchValueFrom struct {
	Template string `json:"template"`
}

type renderedDocument struct {
	Kind string            `json:"kind"`
	Data map[string]string `json:"data"`
	Spec classSpec         `json:"spec"`
}

type classSpec struct {
	Patches []classPatch `json:"patches"`
}

func renderTalosClass(t *testing.T) talosClass {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/talos.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	class := talosClass{templates: map[string]map[string]any{}}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var head renderedDocument
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatal(err)
		}
		switch head.Kind {
		case "ClusterClass":
			class.patches = head.Spec.Patches
		case "ConfigMap":
			for tpl := range strings.SplitSeq(head.Data["templates.yaml"], "\n---\n") {
				var obj map[string]any
				if err := yaml.Unmarshal([]byte(tpl), &obj); err != nil {
					t.Fatal(err)
				}
				kind, ok := obj["kind"].(string)
				if !ok {
					t.Fatalf("a template without a kind: %v", obj)
				}
				class.templates[kind] = obj
			}
		}
	}
	if class.patches == nil || len(class.templates) != 2 {
		t.Fatalf("rendered %d patches and templates %v", len(class.patches), class.templates)
	}
	return class
}

// capiTemplate renders a template as Cluster API renders patches, with the cluster's variables.
func capiTemplate(t *testing.T, text string, variables map[string]any) string {
	t.Helper()
	tpl, err := template.New("patch").Funcs(sprig.HermeticTxtFuncMap()).Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, variables); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// strategicPatches are a template's own strategic patches with those the class's inline patches
// add for a cluster of these variables, in order.
func (c talosClass) strategicPatches(t *testing.T, kind string, variables map[string]any) []string {
	t.Helper()
	path := []string{"spec", "template", "spec"}
	if kind == "TalosControlPlaneTemplate" {
		path = append(path, "controlPlaneConfig", "controlplane")
	}
	own, ok := field(t, c.templates[kind], append(path, "strategicPatches")...).([]any)
	if !ok {
		t.Fatalf("%s has no strategic patches", kind)
	}
	var patches []string
	for _, p := range own {
		s, ok := p.(string)
		if !ok {
			t.Fatalf("%s: a strategic patch of %T", kind, p)
		}
		patches = append(patches, s)
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
				if jp.Op != "add" || !strings.HasSuffix(jp.Path, "/strategicPatches/-") {
					t.Fatalf("patch %s: %s %s is not a strategic patch", p.Name, jp.Op, jp.Path)
				}
				value := jp.Value
				if jp.ValueFrom != nil {
					if err := yaml.Unmarshal([]byte(capiTemplate(t, jp.ValueFrom.Template, variables)), &value); err != nil {
						t.Fatalf("patch %s: %v", p.Name, err)
					}
				}
				s, ok := value.(string)
				if !ok {
					t.Fatalf("patch %s: %T, not a string", p.Name, value)
				}
				patches = append(patches, s)
			}
		}
	}
	return patches
}

type metalMode struct{}

func (metalMode) String() string        { return "metal" }
func (metalMode) RequiresInstall() bool { return false }
func (metalMode) InContainer() bool     { return false }

// talosConfig generates a node's configuration as the Talos bootstrap provider does, applies the
// patches after the user's as it does its own, checks it as talosctl would, and returns its
// documents by kind and name.
func talosConfig(t *testing.T, role machine.Type, patches []string) map[string]map[string]any {
	t.Helper()
	contract, err := config.ParseContractFromVersion("v1.14")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.NewBundle(secrets.NewClock(), contract)
	if err != nil {
		t.Fatal(err)
	}
	in, err := generate.NewInput("demo", "https://192.0.2.10:6443", "1.36.2", generate.WithVersionContract(contract), generate.WithSecretsBundle(bundle))
	if err != nil {
		t.Fatal(err)
	}
	generated, err := in.Config(role)
	if err != nil {
		t.Fatal(err)
	}
	data, err := generated.EncodeString(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		t.Fatal(err)
	}
	hostname, err := stdpatches.WithStaticHostname(contract, "demo-worker-abcde")
	if err != nil {
		t.Fatal(err)
	}
	loaded := make([]configpatcher.Patch, 0, len(patches)+1)
	for _, p := range append(slices.Clone(patches), string(hostname)) {
		patch, loadErr := configpatcher.LoadPatch([]byte(p))
		if loadErr != nil {
			t.Fatalf("%v:\n%s", loadErr, p)
		}
		loaded = append(loaded, patch)
	}
	out, err := configpatcher.Apply(configpatcher.WithBytes([]byte(data)), loaded)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := out.Config()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cfg.ValidateAsClient(metalMode{}); err != nil {
		t.Fatal(err)
	}
	encoded, err := cfg.EncodeString(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]map[string]any{}
	for doc := range strings.SplitSeq(encoded, "\n---\n") {
		var d map[string]any
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatal(err)
		}
		key, ok := d["kind"].(string)
		if !ok {
			key, ok = d["version"].(string)
		}
		if !ok {
			t.Fatalf("a document with neither kind nor version: %v", d)
		}
		if name, ok := d["name"].(string); ok {
			key += "/" + name
		}
		docs[key] = d
	}
	return docs
}

func field(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var v any = doc
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%v: no %s", path, p)
		}
		v = m[p]
	}
	return v
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestTalosControlPlaneConfiguration(t *testing.T) {
	t.Parallel()
	class := renderTalosClass(t)
	for _, c := range []struct {
		name      string
		variables map[string]any
		flannel   bool
		enforce   string
	}{
		{"flannel", map[string]any{"controlPlaneEndpoint": "192.0.2.10", "cni": "flannel"}, true, "privileged"},
		{"cilium, CIS", map[string]any{"controlPlaneEndpoint": "192.0.2.10", "cni": "cilium", "cis": true}, false, "restricted"},
		{"calico, OIDC", map[string]any{
			"controlPlaneEndpoint": "192.0.2.10", "cni": "calico", "podSecurity": map[string]any{"admission": "baseline"},
			"oidc": []any{map[string]any{"issuerURL": "https://idp.example", "clientID": "kubernetes"}},
		}, false, "baseline"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			docs := talosConfig(t, machine.TypeControlPlane, class.strategicPatches(t, "TalosControlPlaneTemplate", c.variables))
			if _, ok := docs["KubeFlannelCNIConfig"]; ok != c.flannel {
				t.Errorf("flannel = %v", ok)
			}
			if _, ok := docs["DiscoveryServiceConfig/default"]; ok {
				t.Error("the discovery service is on")
			}
			if got := field(t, docs["KubeAdmissionControlConfig/PodSecurity"], "configuration", "defaults", "enforce"); got != c.enforce {
				t.Errorf("Pod Security Admission enforces %v", got)
			}
			if got := field(t, docs["KubeAdmissionControlConfig/EventRateLimit"], "configuration", "kind"); got != "Configuration" {
				t.Errorf("EventRateLimit = %v", docs["KubeAdmissionControlConfig/EventRateLimit"])
			}
			anonymous := asJSON(t, field(t, docs["KubeAuthenticationConfig"], "configuration", "anonymous"))
			if anonymous != `{"conditions":[{"path":"/livez"},{"path":"/readyz"},{"path":"/healthz"}],"enabled":true}` {
				t.Errorf("anonymous = %s", anonymous)
			}
			jwt := asJSON(t, field(t, docs["KubeAuthenticationConfig"], "configuration", "jwt"))
			if _, oidc := c.variables["oidc"]; oidc != strings.Contains(jwt, "https://idp.example") {
				t.Errorf("jwt = %s", jwt)
			}
			if got := asJSON(t, field(t, docs["KubeAuditPolicyConfig"], "configuration", "omitStages")); got != `["RequestReceived"]` {
				t.Errorf("audit policy = %v", docs["KubeAuditPolicyConfig"])
			}
			if got := field(t, docs["Layer2VIPConfig/192.0.2.10"], "link"); got != "lan" {
				t.Errorf("virtual IP on %v", got)
			}
			if got := field(t, docs["LinkAliasConfig/lan"], "selector", "match"); got != `link.driver == "virtio_net"` {
				t.Errorf("link alias selects %v", got)
			}
			args, ok := field(t, docs["KubeAPIServerConfig"], "extraArgs").(map[string]any)
			if !ok || args["enable-admission-plugins"] != "EventRateLimit" || args["kubelet-certificate-authority"] == nil {
				t.Errorf("API server arguments = %v", args)
			}
			if got := field(t, docs["KubeControllerManagerConfig"], "extraArgs", "leader-elect-lease-duration"); got != "60s" {
				t.Errorf("controller manager lease = %v", got)
			}
			if got := field(t, docs["v1alpha1"], "cluster", "etcd", "extraArgs", "listen-metrics-urls"); got != "http://127.0.0.1:2381" {
				t.Errorf("etcd metrics on %v", got)
			}
			checkTalosCommon(t, docs)
		})
	}
}

func TestTalosWorkerConfiguration(t *testing.T) {
	t.Parallel()
	class := renderTalosClass(t)
	docs := talosConfig(t, machine.TypeWorker, class.strategicPatches(t, "TalosConfigTemplate", map[string]any{"cni": "cilium"}))
	if _, ok := docs["DiscoveryServiceConfig/default"]; ok {
		t.Error("the discovery service is on")
	}
	checkTalosCommon(t, docs)
}

func checkTalosCommon(t *testing.T, docs map[string]map[string]any) {
	t.Helper()
	if on, ok := field(t, docs["v1alpha1"], "cluster", "externalCloudProvider", "enabled").(bool); !ok || !on {
		t.Errorf("external cloud provider = %v", docs["v1alpha1"]["cluster"])
	}
	if on, ok := field(t, docs["KubeletConfig"], "config", "serverTLSBootstrap").(bool); !ok || !on {
		t.Errorf("kubelet serving certificate bootstrap = %v", docs["KubeletConfig"])
	}
	rules := asJSON(t, field(t, docs["ImageVerificationConfig"], "rules"))
	for _, registry := range []string{"factory.talos.dev/*", "ghcr.io/siderolabs/*", "registry.k8s.io/*"} {
		if !strings.Contains(rules, `"image":"`+registry+`"`) {
			t.Errorf("no signature rule for %s: %s", registry, rules)
		}
	}
	if got := field(t, docs["HostnameConfig"], "hostname"); got != "demo-worker-abcde" {
		t.Errorf("hostname = %v", got)
	}
}
