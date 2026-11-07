package provisioner

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// requireAlloyEnv makes a missing alloy a failure, as make test sets it: the generated
// configurations are only proven to parse by the pinned Alloy (scripts/tools.sh).
const requireAlloyEnv = "KUBEHENGE_REQUIRE_ALLOY"

// alloyBinary finds the pinned Alloy, or skips the test where there is none.
func alloyBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("alloy")
	if err != nil {
		if os.Getenv(requireAlloyEnv) != "" {
			t.Fatalf("alloy is not on PATH, and %s asks for it: install it with scripts/tools.sh", requireAlloyEnv)
		}
		t.Skip("alloy is not on PATH: scripts/tools.sh installs the pinned release, which proves the generated configurations parse")
	}
	return path
}

// validateAlloy runs the config through `alloy validate`, which checks syntax, components,
// references, required and unknown arguments at the stability level the chart starts Alloy with.
func validateAlloy(t *testing.T, binary, name string, values map[string]any) {
	t.Helper()
	// Two template passes reach Alloy: the add-on provider renders the values, then the chart
	// renders the configuration's content, which is why alloyCommon escapes the braces of Alloy's own templates.
	config := helmRendered(t, helmRendered(t, lookup[string](t, values, "alloy", "configMap", "content")))
	level := "generally-available"
	if l, ok := lookup[map[string]any](t, values, "alloy")["stabilityLevel"].(string); ok {
		level = l
	}
	file := filepath.Join(t.TempDir(), name+".alloy")
	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // the binary is the pinned alloy found on PATH, and the file is this test's own
	out, err := exec.CommandContext(t.Context(), binary, "validate", "--stability.level="+level, file).CombinedOutput()
	if err != nil {
		t.Errorf("%s does not validate at %s: %v\n%s\n%s", name, level, err, out, config)
	}
}

func TestAlloyToolIsTheChartsRelease(t *testing.T) {
	t.Parallel()
	tools, err := os.ReadFile("../../scripts/tools.sh")
	if err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile("../../charts/kubehenge/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`(?m)^ALLOY=(\S+)$`).FindSubmatch(tools)
	image := regexp.MustCompile(`grafana/alloy:(v[0-9.]+)@sha256`).FindSubmatch(values)
	if pinned == nil || image == nil {
		t.Fatalf("no Alloy pin in scripts/tools.sh (%q) or the chart's values (%q)", pinned, image)
	}
	if !bytes.Equal(pinned[1], image[1]) {
		t.Errorf("scripts/tools.sh validates with Alloy %s, the chart runs %s", pinned[1], image[1])
	}
}

func TestGeneratedAlloyConfigurationsValidate(t *testing.T) {
	t.Parallel()
	binary := alloyBinary(t)
	bundle := testCABundle(t)
	var everyKind []LogOutput
	for _, kind := range []string{logOutputSyslog, logOutputOTLP, logOutputLoki} {
		endpoint := map[string]string{logOutputSyslog: "tls://s.example.com:6514", logOutputOTLP: "https://o.example.com:4318", logOutputLoki: "https://l.example.com/loki/api/v1/push"}[kind]
		for _, auth := range logAuths {
			if kind == logOutputSyslog && auth != logAuthNone {
				continue
			}
			everyKind = append(everyKind,
				LogOutput{Name: kind + "-" + auth, Kind: kind, Endpoint: endpoint, Auth: auth},
				LogOutput{Name: kind + "-" + auth + "-ca", Kind: kind, Endpoint: endpoint, Auth: auth, CABundle: bundle, Streams: []string{streamAudit, streamNodes}},
			)
		}
	}
	plain := []LogOutput{
		{Name: "tcp", Kind: logOutputSyslog, Endpoint: "tcp://s.example.com:514"},
		{Name: "udp", Kind: logOutputSyslog, Endpoint: "udp://s.example.com:514", Streams: []string{streamContainers}},
		{Name: "otlp-http", Kind: logOutputOTLP, Endpoint: "http://o.example.com:4318", Streams: []string{streamEvents}},
	}
	scenarios := map[string][]LogOutput{"no-outputs": nil, "plain": plain}
	// Four outputs at most go into one cluster, so every output is checked in groups of four.
	for i := 0; i < len(everyKind); i += maxLogOutputs {
		scenarios["group-"+strings.Repeat("x", i/maxLogOutputs)] = everyKind[i:min(i+maxLogOutputs, len(everyKind))]
	}
	for name, outputs := range scenarios {
		for _, collector := range []string{collectorNode, collectorAPI} {
			for _, distribution := range []templates.Distribution{templates.Kubeadm, templates.Talos} {
				t.Run(name+"/"+collector+"/"+string(distribution), func(t *testing.T) {
					t.Parallel()
					addons := logsOnWith(Logs{Collector: collector, Outputs: outputs})
					var values map[string]map[string]any
					if distribution == templates.Talos {
						values = talosProxies(t, CNI{}, addons)
					} else {
						values, _ = proxiesOf(t, distribution, addons)
					}
					for _, release := range []string{"web-alloy", "web-alloy-node"} {
						if v, ok := values[release]; ok {
							validateAlloy(t, binary, release, v)
						}
					}
				})
			}
		}
	}
}
