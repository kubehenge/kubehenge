package kubehenge_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestControllerGetsTheReleaseMirror(t *testing.T) {
	t.Parallel()
	args := controllerArgs(t, "releaseMirror.url=https://mirror.example.org/github")
	if !slices.Contains(args, "--release-mirror=https://mirror.example.org/github") {
		t.Errorf("the controller lacks the release mirror in %v", args)
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--release-mirror-ca") }) {
		t.Errorf("the controller got an authority with none set: %v", args)
	}
}

func TestControllerGetsNoReleaseMirrorUnlessSet(t *testing.T) {
	t.Parallel()
	for _, a := range controllerArgs(t) {
		if strings.HasPrefix(a, "--release-mirror") {
			t.Errorf("the controller got %s with no mirror set", a)
		}
	}
	if out := helmTemplate(t, "templates/controller.yaml"); strings.Contains(out, "kubehenge-release-mirror") {
		t.Error("the chart renders the mirror's authority with none set")
	}
}

func TestControllerMountsTheReleaseMirrorsAuthority(t *testing.T) {
	t.Parallel()
	set := []string{"releaseMirror.url=https://mirror.example.org/github", "releaseMirror.caBundle=-----BEGIN CERTIFICATE-----"}
	if args := controllerArgs(t, set...); !slices.Contains(args, "--release-mirror-ca=/etc/kubehenge/release-mirror/ca.crt") {
		t.Errorf("the controller lacks the mirror's authority in %v", args)
	}
	out := helmTemplate(t, "templates/controller.yaml", set...)
	for _, want := range []string{
		"name: kubehenge-release-mirror", `ca.crt: "-----BEGIN CERTIFICATE-----"`,
		"mountPath: /etc/kubehenge/release-mirror", "kubehenge.dev/release-mirror-ca:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered controller lacks %q", want)
		}
	}
}

func TestChartRefusesAReleaseMirrorThatIsNoAddress(t *testing.T) {
	t.Parallel()
	for _, mirror := range []string{"mirror.example.org", "https://mirror.example.org/", "https://mirror.example.org/a b", "oci://mirror.example.org"} {
		cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
			"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--set-string", "releaseMirror.url="+mirror)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("the release mirror %q was accepted:\n%.300s", mirror, out)
		}
	}
}
