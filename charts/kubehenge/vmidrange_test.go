package kubehenge_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

var classFiles = []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"}

func TestClusterVMIDRangeReachesCapmoxAndNoRangeLeavesNextID(t *testing.T) {
	t.Parallel()
	for _, file := range classFiles {
		class := renderClass(t, file)
		if _, ok := class.variables["vmIDRange"]; !ok {
			t.Errorf("%s declares no vmIDRange variable", file)
		}
		set := asMap(t, field(t, class.patched(t, "ProxmoxMachineTemplate", variablesOf(t, `{"vmIDRange": {"start": 10000, "end": 10999}}`)), "spec", "template", "spec"))
		want := map[string]any{"start": float64(10000), "end": float64(10999)}
		if got := set["vmIDRange"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the machines' vmIDRange is %v, want %v", file, got, want)
		}
		unset := asMap(t, field(t, class.patched(t, "ProxmoxMachineTemplate", variablesOf(t, `{}`)), "spec", "template", "spec"))
		if got, ok := unset["vmIDRange"]; ok {
			t.Errorf("%s: a cluster without a range has vmIDRange %v, so capmox would not take Proxmox VE's next ID", file, got)
		}
	}
}

func TestSiteVMIDRangeIsNamedOnTheClassesAndGivenToTheTemplateBuilds(t *testing.T) {
	t.Parallel()
	site := map[string]any{"vmIDRange": map[string]any{"start": 10000, "end": 10999}}
	for _, file := range classFiles {
		if out := helmTemplateWith(t, "templates/"+file, site); !strings.Contains(out, `kubehenge.dev/vm-id-range: "10000-10999"`) {
			t.Errorf("%s does not name the site's VM ID range for the interface and the webhook", file)
		}
		if out := helmTemplateWith(t, "templates/"+file, map[string]any{}); strings.Contains(out, "kubehenge.dev/vm-id-range") {
			t.Errorf("%s names a site VM ID range nobody set", file)
		}
	}
	if out := helmTemplateWith(t, "templates/templaterefresh.yaml", site); !strings.Contains(out, "--vm-id-range=10000-10999") {
		t.Error("the template builds are not given the site's VM ID range")
	}
	if out := helmTemplateWith(t, "templates/templaterefresh.yaml", map[string]any{}); strings.Contains(out, "--vm-id-range") {
		t.Error("the template builds are given a VM ID range nobody set")
	}
}

func TestChartRefusesAHalfSetVMIDRange(t *testing.T) {
	t.Parallel()
	for _, set := range []string{"vmIDRange.start=10000", "vmIDRange.end=10999", "vmIDRange.start=99,vmIDRange.end=1000"} {
		cmd := exec.CommandContext(t.Context(), "helm", "template", //nolint:gosec // helm with the test's own arguments
			"kubehenge", ".", "--namespace", "kubehenge-system", "--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--set", set)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%q was accepted:\n%.300s", set, out)
		}
	}
}
