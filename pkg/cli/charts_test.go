package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	pinDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pinValues = `addons:
  cloudControllerManager:
    repository: oci://ghcr.io/sergelogvinov/charts
    chart: proxmox-cloud-controller-manager
    version: 0.2.32@sha256:` + pinDigest + `
  certManager:
    repository: https://charts.jetstack.io
    chart: cert-manager
    version: v1.21.3@sha256:` + pinDigest + `
  rancher:
    kubernetes: 34-36
`
)

func writeValues(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte(pinValues), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestChartsListsEveryPinnedChartToMirror(t *testing.T) {
	t.Parallel()
	out, err := run(t, "charts", writeValues(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://charts.jetstack.io/cert-manager:v1.21.3@sha256:" + pinDigest + "\n" +
		"oci://ghcr.io/sergelogvinov/charts/proxmox-cloud-controller-manager:0.2.32@sha256:" + pinDigest + "\n"
	if out != want {
		t.Errorf("charts printed\n%s\nwant\n%s", out, want)
	}
}

func TestChartsShowsWhereAMirrorServesThem(t *testing.T) {
	t.Parallel()
	out, err := run(t, "charts", writeValues(t), "--mirror", "ghcr.io=oci://registry.example.org/ghcr")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\toci://registry.example.org/ghcr/sergelogvinov/charts/proxmox-cloud-controller-manager\n") {
		t.Errorf("the mirrored chart is not shown with its new place:\n%s", out)
	}
	if strings.Count(out, "\t") != 1 {
		t.Errorf("a chart no mirror covers got a place:\n%s", out)
	}
}

func TestChartsRefusesAMirrorOfAnotherKind(t *testing.T) {
	t.Parallel()
	_, err := run(t, "charts", writeValues(t), "--mirror", "ghcr.io=https://registry.example.org/ghcr")
	if err == nil || !strings.Contains(err.Error(), "oci://") {
		t.Errorf("an HTTP mirror of an OCI host gave %v", err)
	}
	_, err = run(t, "charts", writeValues(t), "--mirror", "quay.io=oci://registry.example.org/q")
	if err == nil || !strings.Contains(err.Error(), "no chart comes from quay.io") {
		t.Errorf("a mirror of a host no chart uses gave %v", err)
	}
}

func TestChartsRefusesValuesWithoutCharts(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte("image: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "charts", file); err == nil || !strings.Contains(err.Error(), "pin no chart") {
		t.Errorf("values without charts gave %v", err)
	}
	if _, err := run(t, "charts", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing file was read")
	}
}

func TestControllerHoldsMirrorsToItsCharts(t *testing.T) {
	t.Parallel()
	ccm, err := provisioner.ParseChart("oci://ghcr.io/sergelogvinov/charts/proxmox-cloud-controller-manager:0.2.32@sha256:" + pinDigest)
	if err != nil {
		t.Fatal(err)
	}
	o := &controllerOptions{chartMirrors: []string{"quay.io=oci://registry.example.org/q"}, egressProxy: "http://proxy.example.org:3128", egressNoProxy: "pve.example.org, .corp.example"}
	o.r.Charts.CCM = ccm
	if err = o.parseMirrors(); err == nil || !strings.Contains(err.Error(), "--chart-mirror") {
		t.Fatalf("a mirror of a host no chart uses gave %v", err)
	}
	o.chartMirrors = []string{"ghcr.io=oci://registry.example.org/ghcr"}
	if err = o.parseMirrors(); err != nil {
		t.Fatal(err)
	}
	if got := o.r.Charts.Mirrors.Repository(ccm.Repository); got != "oci://registry.example.org/ghcr/sergelogvinov/charts" {
		t.Errorf("the controller fetches from %s", got)
	}
	if o.r.Egress.URL != "http://proxy.example.org:3128" || strings.Join(o.r.Egress.NoProxy, "|") != "pve.example.org|.corp.example" {
		t.Errorf("the default proxy %+v", o.r.Egress)
	}
}
