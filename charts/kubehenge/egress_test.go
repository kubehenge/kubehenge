package kubehenge_test

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func helmTemplate(t *testing.T, show string, set ...string) string {
	t.Helper()
	args := make([]string, 0, 9+2*len(set))
	args = append(args,
		"template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", show,
	)
	for _, s := range set {
		args = append(args, "--set", s)
	}
	cmd := exec.CommandContext(t.Context(), "helm", args...) //nolint:gosec // helm with the test's own arguments
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	return string(out)
}

type argsContainer struct {
	Args []string `json:"args"`
}

type argsPodSpec struct {
	Containers []argsContainer `json:"containers"`
}

type argsPodTemplate struct {
	Spec argsPodSpec `json:"spec"`
}

type argsDeploymentSpec struct {
	Template argsPodTemplate `json:"template"`
}

type argsDeployment struct {
	Kind string             `json:"kind"`
	Spec argsDeploymentSpec `json:"spec"`
}

func controllerArgs(t *testing.T, set ...string) []string {
	t.Helper()
	for doc := range strings.SplitSeq(helmTemplate(t, "templates/controller.yaml", set...), "\n---\n") {
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

func TestControllerGetsEveryChartMirror(t *testing.T) {
	t.Parallel()
	args := controllerArgs(t, `chartMirrors.ghcr\.io=oci://registry.example.org/ghcr`, `chartMirrors.charts\.jetstack\.io=https://charts.example.org/jetstack`)
	for _, want := range []string{
		"--chart-mirror=ghcr.io=oci://registry.example.org/ghcr",
		"--chart-mirror=charts.jetstack.io=https://charts.example.org/jetstack",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("the controller lacks %s in %v", want, args)
		}
	}
}

func TestControllerGetsNoMirrorUnlessSet(t *testing.T) {
	t.Parallel()
	for _, a := range controllerArgs(t) {
		if strings.HasPrefix(a, "--chart-mirror") {
			t.Errorf("the controller got %s with no mirror set", a)
		}
	}
}

func TestChartRefusesAMirrorThatIsNoAddress(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--set", `chartMirrors.ghcr\.io=registry.example.org`)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a mirror without a scheme was accepted:\n%s", out)
	}
}

func TestEveryClassOffersTheEgressProxyAndTheRepositoryAuthority(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"kubeadm", "rke2", "k3s", "talos"} {
		out := helmTemplate(t, "templates/"+class+".yaml")
		for _, want := range []string{"name: egressProxy", "caBundle:", "A cluster that goes direct names no proxy.", "Flux reads the authority from ca.crt in your own Secret."} {
			if !strings.Contains(out, want) {
				t.Errorf("the %s class lacks %q", class, want)
			}
		}
	}
}
