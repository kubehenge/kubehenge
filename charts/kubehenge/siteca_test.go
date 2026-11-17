package kubehenge_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"sigs.k8s.io/yaml"
)

// helmTemplateWith renders one template of the chart with values that need more than --set can
// carry, such as a certificate's newlines.
func helmTemplateWith(t *testing.T, show string, values map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "values.json")
	if err = os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system", //nolint:gosec // helm with the test's own arguments
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--values", file, "--show-only", show)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, out)
	}
	return string(out)
}

func deploymentArgs(t *testing.T, rendered string) []string {
	t.Helper()
	for doc := range strings.SplitSeq(rendered, "\n---\n") {
		var d argsDeployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatal(err)
		}
		if d.Kind == "Deployment" {
			return d.Spec.Template.Spec.Containers[0].Args
		}
	}
	t.Fatal("no Deployment in the rendered controller")
	return nil
}

// certificateBody is a line of the certificate's base64, which the nodes' files carry.
func certificateBody(pem string) string {
	return strings.Split(pem, "\n")[1]
}

func TestNodesTrustTheSiteCAsTheControllerGaveThem(t *testing.T) {
	t.Parallel()
	site, own := siteCA(t, "site"), siteCA(t, "own")
	for _, c := range siteCases {
		render := func(vars map[string]any) string {
			raw, err := json.Marshal(field(t, renderClass(t, c.file).patched(t, c.kind, vars), c.files...))
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
		withSite := render(variablesOf(t, `{"siteCAs": [`+jsonString(site)+`]}`))
		if !strings.Contains(withSite, "node-site") && !strings.Contains(withSite, "TrustedRootsConfig") {
			t.Errorf("%s %s: the site CA writes no trust configuration", c.file, c.kind)
		}
		if !strings.Contains(withSite, certificateBody(site)) {
			t.Errorf("%s %s: the nodes lack the site CA", c.file, c.kind)
		}
		withOwn := render(variablesOf(t, `{"trustedCAs": [`+jsonString(own)+`]}`))
		if !strings.Contains(withOwn, certificateBody(own)) || strings.Contains(withOwn, certificateBody(site)) {
			t.Errorf("%s %s: a cluster's own trustedCAs do not stand alone", c.file, c.kind)
		}
		bare := render(variablesOf(t, `{"siteCAs": []}`))
		if strings.Contains(bare, "node-site") || strings.Contains(bare, "TrustedRootsConfig") {
			t.Errorf("%s %s: no site CA still writes a trust configuration", c.file, c.kind)
		}
	}
}

func TestTalosNodesTrustTheSiteCAAndEveryClassTakesTheVariable(t *testing.T) {
	t.Parallel()
	site := siteCA(t, "site")
	class := renderTalosClass(t)
	for _, kind := range []string{"TalosControlPlaneTemplate", "TalosConfigTemplate"} {
		role := machine.TypeControlPlane
		if kind == "TalosConfigTemplate" {
			role = machine.TypeWorker
		}
		docs := talosConfig(t, role, class.strategicPatches(t, kind, variablesOf(t, `{"controlPlaneEndpoint": "192.0.2.10", "siteCAs": [`+jsonString(site)+`]}`)))
		roots, ok := docs["TrustedRootsConfig/kubehenge"]["certificates"].(string)
		if !ok || !strings.Contains(roots, certificateBody(site)) {
			t.Errorf("%s: trusted roots %v lack the site CA", kind, docs["TrustedRootsConfig/kubehenge"])
		}
	}
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"} {
		if _, ok := renderClass(t, file).variables["siteCAs"]; !ok {
			t.Errorf("%s declares no siteCAs variable, so a cluster could not carry one", file)
		}
	}
}

func TestControllerGetsTheSiteCAAndTimeZone(t *testing.T) {
	t.Parallel()
	site := siteCA(t, "site")
	out := helmTemplateWith(t, "templates/controller.yaml", map[string]any{"siteCA": site, "timeZone": "Europe/Zurich"})
	args := deploymentArgs(t, out)
	for _, want := range []string{"--site-ca=/etc/kubehenge/site-ca/ca.crt", "--time-zone=Europe/Zurich"} {
		if !slices.Contains(args, want) {
			t.Errorf("the controller lacks %s in %v", want, args)
		}
	}
	// The ConfigMap and the volume's source, then the volume and the mount that name it.
	if got, mounted := strings.Count(out, "kubehenge-site-ca"), strings.Count(out, "name: site-ca"); got != 2 || mounted != 2 {
		t.Errorf("the ConfigMap is named %d times and mounted by %d entries, want 2 and 2", got, mounted)
	}
	for _, want := range []string{"mountPath: /etc/kubehenge/site-ca", "kubehenge.dev/site-ca:", "ca.crt:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered controller lacks %q", want)
		}
	}
	plain := controllerArgs(t)
	if slices.ContainsFunc(plain, func(a string) bool { return strings.HasPrefix(a, "--site-ca") || strings.HasPrefix(a, "--time-zone") }) {
		t.Errorf("with nothing set the controller got %v", plain)
	}
}

func TestNoSiteTimeZoneLeavesTheWizardTheBrowsersAndSchedulesToTheController(t *testing.T) {
	t.Parallel()
	out := helmTemplateWith(t, "templates/rke2.yaml", map[string]any{})
	if strings.Contains(out, "kubehenge.dev/time-zone") {
		t.Error("the class names a site time zone nobody set")
	}
	if got := strings.Count(out, `default: "UTC"`); got != 0 {
		t.Errorf("%d schedule defaults are UTC, want none: the controller reads a schedule naming no zone in the site's", got)
	}
}

func TestOwnCAsStandAloneSoDroppingTheSiteCAChangesNothing(t *testing.T) {
	t.Parallel()
	site, own := siteCA(t, "site"), siteCA(t, "own")
	for _, c := range siteCases {
		render := func(vars string) string {
			raw, err := json.Marshal(field(t, renderClass(t, c.file).patched(t, c.kind, variablesOf(t, vars)), c.files...))
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
		both := render(`{"siteCAs": [` + jsonString(site) + `], "trustedCAs": [` + jsonString(own) + `]}`)
		alone := render(`{"trustedCAs": [` + jsonString(own) + `]}`)
		if both != alone {
			t.Errorf("%s %s: a siteCAs variable beside trustedCAs changes the nodes", c.file, c.kind)
		}
	}
}

func TestSchedulesInTheClassTakeNoSiteTimeZoneTheControllerCannotCheck(t *testing.T) {
	t.Parallel()
	// An unknown zone in the class defaults would have admission refuse every cluster with a
	// schedule. The controller resolves a schedule naming none at run time, UTC for an unknown zone.
	for _, zone := range []string{"Pacific/Auckland", "Mars/Olympus"} {
		out := helmTemplateWith(t, "templates/rke2.yaml", map[string]any{"timeZone": zone})
		if got := strings.Count(out, `default: "`+zone+`"`); got != 0 {
			t.Errorf("%d schedule defaults name the site time zone %s, want none", got, zone)
		}
	}
	out := helmTemplateWith(t, "templates/rke2.yaml", map[string]any{"timeZone": "Pacific/Auckland"})
	if !strings.Contains(out, `kubehenge.dev/time-zone: "Pacific/Auckland"`) {
		t.Error("the class does not name the site time zone for the interface")
	}
}

func TestChartRefusesAMalformedSiteCAAndTimeZone(t *testing.T) {
	t.Parallel()
	for _, set := range []string{"siteCA=hello", "siteCA=-----BEGIN PRIVATE KEY-----", "timeZone=Local", "timeZone=Mars Olympus"} {
		cmd := exec.CommandContext(t.Context(), "helm", "template", //nolint:gosec // helm with the test's own arguments
			"kubehenge", ".", "--namespace", "kubehenge-system", "--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--set-string", set)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%q was accepted:\n%.300s", set, out)
		}
	}
}

func TestIdentityProvidersWithoutACAGetTheSiteCA(t *testing.T) {
	t.Parallel()
	site, own := siteCA(t, "site"), siteCA(t, "own")
	providers := []map[string]any{
		{"issuerURL": "https://own.example.org", "clientID": "a", "certificateAuthority": own},
		{"issuerURL": "https://site.example.org", "clientID": "b"},
	}
	out := helmTemplateWith(t, "templates/rke2.yaml", map[string]any{"siteCA": site, "clusterOIDC": providers})
	if !strings.Contains(out, certificateBody(site)) {
		t.Error("the provider without a CA did not get the site CA")
	}
	if !strings.Contains(out, certificateBody(own)) {
		t.Error("a provider's own CA was dropped")
	}
	if without := helmTemplateWith(t, "templates/rke2.yaml", map[string]any{"clusterOIDC": providers}); strings.Contains(without, certificateBody(site)) {
		t.Error("the site CA showed up although none is set")
	}
}

func TestBackupTargetsAndIdentityProvidersInheritTheSiteCA(t *testing.T) {
	t.Parallel()
	site, own := siteCA(t, "site"), siteCA(t, "own")
	out := helmTemplateWith(t, "templates/rke2.yaml", map[string]any{
		"siteCA": site,
		"backupTargets": []map[string]any{
			{"name": "own", "bucket": "backups", "endpoint": "https://s3.example.org", "caCertificate": own},
			{"name": "site", "bucket": "backups", "endpoint": "https://s3.example.org"},
			{"name": "aws", "bucket": "backups"},
		},
	})
	var annotation string
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "kubehenge.dev/backup-targets: "); ok {
			annotation = rest
		}
	}
	var targets []map[string]any
	unquoted := ""
	if err := json.Unmarshal([]byte(annotation), &unquoted); err != nil || json.Unmarshal([]byte(unquoted), &targets) != nil || len(targets) != 3 {
		t.Fatalf("backup targets annotation %q: %v", annotation, err)
	}
	if targets[0]["caCertificate"] != own {
		t.Errorf("a target's own CA = %v, want it kept", targets[0]["caCertificate"])
	}
	if got := fmt.Sprint(targets[1]["caCertificate"]); !strings.Contains(got, certificateBody(site)) {
		t.Errorf("an endpoint without a CA = %q, want the site CA", got)
	}
	if _, has := targets[2]["caCertificate"]; has {
		t.Errorf("a target without an endpoint got a CA: %v", targets[2])
	}
	if !strings.Contains(out, "kubehenge.dev/site-ca: \"set\"") {
		t.Error("the class does not say the site has a CA")
	}
}
