package kubehenge_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	for _, want := range []string{"name: kubehenge-site-ca", "mountPath: /etc/kubehenge/site-ca", "kubehenge.dev/site-ca:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered controller lacks %q", want)
		}
	}
	plain := controllerArgs(t)
	if slices.ContainsFunc(plain, func(a string) bool { return strings.HasPrefix(a, "--site-ca") }) || !slices.Contains(plain, "--time-zone=UTC") {
		t.Errorf("with nothing set the controller got %v", plain)
	}
}

func TestChartRefusesAMalformedSiteCAAndTimeZone(t *testing.T) {
	t.Parallel()
	for _, set := range []string{"siteCA=hello", "siteCA=-----BEGIN PRIVATE KEY-----", "timeZone=", "timeZone=Mars Olympus"} {
		cmd := exec.CommandContext(t.Context(), "helm", "template", //nolint:gosec // helm with the test's own arguments
			"kubehenge", ".", "--namespace", "kubehenge-system", "--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--set-string", set)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%q was accepted:\n%.300s", set, out)
		}
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
	if got, _ := targets[1]["caCertificate"].(string); !strings.Contains(got, certificateBody(site)) {
		t.Errorf("an endpoint without a CA = %q, want the site CA", got)
	}
	if _, has := targets[2]["caCertificate"]; has {
		t.Errorf("a target without an endpoint got a CA: %v", targets[2])
	}
	if !strings.Contains(out, "kubehenge.dev/site-ca: \"set\"") {
		t.Error("the class does not say the site has a CA")
	}
}
