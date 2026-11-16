package kubehenge_test

import (
	"fmt"
	"os/exec"
	"reflect"
	"slices"
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

func TestClusterVMIDRangePatchesSelectTheControlPlaneAndTheWorkers(t *testing.T) {
	t.Parallel()
	for _, file := range classFiles {
		var patch *classPatch
		class := renderClass(t, file)
		for i := range class.patches {
			if class.patches[i].Name == "proxmox-vm-ids" {
				patch = &class.patches[i]
			}
		}
		if patch == nil {
			t.Fatalf("%s has no proxmox-vm-ids patch", file)
		}
		var controlPlane, workers bool
		for _, d := range patch.Definitions {
			if d.Selector.Kind != "ProxmoxMachineTemplate" {
				t.Errorf("%s: a proxmox-vm-ids definition selects %q", file, d.Selector.Kind)
			}
			controlPlane = controlPlane || d.Selector.MatchResources.ControlPlane
			workers = workers || slices.Contains(d.Selector.MatchResources.MachineDeployment.Names, "worker")
		}
		if !controlPlane || !workers {
			t.Errorf("%s: the VM ID range reaches the control plane %v and the workers %v", file, controlPlane, workers)
		}
	}
}

func TestSiteVMIDRangeIsNamedOnTheClassesAndGivenToTheTemplateBuilds(t *testing.T) {
	t.Parallel()
	for _, r := range [][2]int{{10000, 10999}, {1000000, 1000999}, {999999000, 999999999}} {
		site := map[string]any{"vmIDRange": map[string]any{"start": r[0], "end": r[1]}}
		text := fmt.Sprintf("%d-%d", r[0], r[1])
		for _, file := range classFiles {
			if out := helmTemplateWith(t, "templates/"+file, site); !strings.Contains(out, `kubehenge.dev/vm-id-range: "`+text+`"`) {
				t.Errorf("%s does not name the site's VM ID range %s for the interface and the webhook", file, text)
			}
		}
		if out := helmTemplateWith(t, "templates/templaterefresh.yaml", site); !strings.Contains(out, "--vm-id-range="+text+"\n") {
			t.Errorf("the template builds are not given the site's VM ID range %s", text)
		}
	}
	for _, file := range classFiles {
		if out := helmTemplateWith(t, "templates/"+file, map[string]any{}); strings.Contains(out, "kubehenge.dev/vm-id-range") {
			t.Errorf("%s names a site VM ID range nobody set", file)
		}
	}
	if out := helmTemplateWith(t, "templates/templaterefresh.yaml", map[string]any{}); strings.Contains(out, "--vm-id-range") {
		t.Error("the template builds are given a VM ID range nobody set")
	}
}

func TestChartRefusesAHalfSetVMIDRange(t *testing.T) {
	t.Parallel()
	for _, set := range []string{"vmIDRange.start=10000", "vmIDRange.end=10999", "vmIDRange.start=99,vmIDRange.end=1000", "vmIDRange.start=20000,vmIDRange.end=10000"} {
		cmd := exec.CommandContext(t.Context(), "helm", "template", //nolint:gosec // helm with the test's own arguments
			"kubehenge", ".", "--namespace", "kubehenge-system", "--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--set", set)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%q was accepted:\n%.300s", set, out)
		}
	}
}
