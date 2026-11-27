package kubehenge_test

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/machine"
)

const (
	managementProxyURL = "http://mgmt.example.org:3128"
	ownProxyURL        = "http://own.example.org:3128"
)

// withManagementProxy are the helm arguments of a management side behind a proxy that re-signs
// TLS, as the module passes them on, and a line of that proxy's authority to look for.
func withManagementProxy(t *testing.T) ([]string, string) {
	t.Helper()
	ca := siteCA(t, "inspecting")
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte(ca), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"--set", "proxy.clusterDefault.url=" + managementProxyURL,
		"--set", "proxy.clusterDefault.noProxy={pve.example.org,10.0.0.0/8}",
		"--set-file", "proxy.clusterDefault.caBundle=" + filepath.ToSlash(path), // helm reads a backslash as an escape
	}, strings.Split(ca, "\n")[1]
}

// egressCases are a cluster's egressProxy: unset, unset with exceptions of its own, its own proxy
// and none.
var egressCases = map[string]string{
	"unset":          `{}`,
	"own exceptions": `{"egressProxy": {"noProxy": [".own.example.org"]}}`,
	"set":            `{"egressProxy": {"url": "` + ownProxyURL + `"}}`,
	"disabled":       `{"egressProxy": {"disabled": true}}`,
}

// egressVariables are bareNet's variables with the case's egressProxy.
func egressVariables(t *testing.T, name string) map[string]any {
	t.Helper()
	vars := variablesOf(t, bareNet)
	maps.Copy(vars, variablesOf(t, egressCases[name]))
	return vars
}

func TestNodesTakeTheManagementProxyByDefault(t *testing.T) {
	t.Parallel()
	set, caLine := withManagementProxy(t)
	classes := map[string]renderedClass{}
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml"} {
		classes[file] = renderClass(t, file, set...)
	}
	for _, c := range siteCases {
		for name := range egressCases {
			spec := asMap(t, field(t, classes[c.file].patched(t, c.kind, egressVariables(t, name)), c.files...))
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			if name == "disabled" {
				if strings.Contains(string(raw), "node-site") || strings.Contains(string(raw), "proxy.env") {
					t.Errorf("%s %s: a cluster that goes direct gets a site script", c.file, c.kind)
				}
				continue
			}
			file, _ := fileOf(t, spec["files"], "/etc/kubehenge/node-site.sh")
			script, ok := file["content"].(string)
			if !ok {
				t.Fatalf("%s %s, egressProxy %s: no site script", c.file, c.kind, name)
			}
			wants := map[string][]string{
				"unset":          {"HTTP_PROXY='" + managementProxyURL + "'\n", "HTTPS_PROXY='" + managementProxyURL + "'\n", ",pve.example.org,10.0.0.0/8'\n", caLine},
				"own exceptions": {"HTTP_PROXY='" + managementProxyURL + "'\n", ",pve.example.org,10.0.0.0/8,.own.example.org'\n", caLine},
				"set":            {"HTTP_PROXY='" + ownProxyURL + "'\n"},
			}[name]
			for _, want := range wants {
				if !strings.Contains(script, want) {
					t.Errorf("%s %s, egressProxy %s: script lacks %q:\n%s", c.file, c.kind, name, want, script)
				}
			}
			if name == "set" && (strings.Contains(script, "pve.example.org") || strings.Contains(script, caLine)) {
				t.Errorf("%s %s: a cluster with its own proxy takes the management side's exceptions or authority:\n%s", c.file, c.kind, script)
			}
			if !strings.Contains(string(raw), "set -a; . /etc/kubehenge/site/proxy.env; set +a") {
				t.Errorf("%s %s, egressProxy %s: the proxy's environment is not read", c.file, c.kind, name)
			}
			syntax(t, script)
		}
	}
}

func TestTalosNodesTakeTheManagementProxyByDefault(t *testing.T) {
	t.Parallel()
	set, caLine := withManagementProxy(t)
	class := renderTalosClass(t, set...)
	for kind, role := range map[string]machine.Type{"TalosControlPlaneTemplate": machine.TypeControlPlane, "TalosConfigTemplate": machine.TypeWorker} {
		for name := range egressCases {
			docs := talosConfig(t, role, class.strategicPatches(t, kind, egressVariables(t, name)))
			env := asJSON(t, field(t, docs["v1alpha1"], "machine", "env"))
			roots, ok := docs["TrustedRootsConfig/kubehenge"]["certificates"].(string)
			if !ok {
				roots = ""
			}
			switch name {
			case "unset", "own exceptions":
				if !strings.Contains(env, `"HTTP_PROXY":"`+managementProxyURL+`"`) || !strings.Contains(env, "pve.example.org,10.0.0.0/8") || !strings.Contains(roots, caLine) {
					t.Errorf("%s, egressProxy %s: env %s, roots %q", kind, name, env, roots)
				}
				if name == "own exceptions" && !strings.Contains(env, "10.0.0.0/8,.own.example.org") {
					t.Errorf("%s: the cluster's own exceptions are not in %s", kind, env)
				}
			case "set":
				if !strings.Contains(env, `"HTTP_PROXY":"`+ownProxyURL+`"`) || strings.Contains(env, "pve.example.org") || roots != "" {
					t.Errorf("%s, egressProxy set: env %s, roots %q", kind, env, roots)
				}
			default:
				if env != "null" || roots != "" {
					t.Errorf("%s, egressProxy disabled: env %s, roots %q", kind, env, roots)
				}
			}
		}
	}
}

// Without a management proxy, an unset egressProxy leaves the nodes as they were.
func TestNodesGoDirectWithoutAManagementProxy(t *testing.T) {
	t.Parallel()
	for _, c := range siteCases {
		spec := asMap(t, field(t, renderClass(t, c.file).patched(t, c.kind, egressVariables(t, "unset")), c.files...))
		if raw, err := json.Marshal(spec); err != nil || strings.Contains(string(raw), "node-site") {
			t.Errorf("%s %s: a site script without any setting (%v)", c.file, c.kind, err)
		}
	}
}

// A management proxy whose address holds credentials never reaches the class, whose node files are
// plain text. The module leaves it out, and the chart refuses it.
func TestChartRefusesACredentialedManagementProxyForNodes(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/k3s.yaml",
		"--set", "proxy.clusterDefault.url=http://user:hunter2@mgmt.example.org:3128")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a proxy address with credentials was accepted:\n%s", out)
	}
}
