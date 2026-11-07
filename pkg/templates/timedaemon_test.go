package templates

import (
	"slices"
	"strings"
	"testing"
)

// A node image names its time daemon as a package and enables it, and the bake proves it answers,
// rather than relying on what the family's minimal image happens to hold.
func TestNodeImagesHoldATimeDaemon(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"el", "leap"} {
		raw, err := recipeFiles.ReadFile(family + "/node.sh")
		if err != nil {
			t.Fatal(err)
		}
		node := string(raw)
		var packages string
		for line := range strings.Lines(node) {
			if strings.HasPrefix(line, "packages=") {
				packages = line
			}
		}
		if !strings.Contains(packages, " chrony ") && !strings.Contains(packages, `"chrony `) {
			t.Errorf("%s/node.sh installs no chrony: %s", family, packages)
		}
		for _, want := range []string{`\nenable ` + timeDaemonUnit + `\n`, "kubehenge-boot-log.timer " + timeDaemon} {
			if !strings.Contains(node, want) {
				t.Errorf("%s/node.sh does not say %s", family, want)
			}
		}
	}
}

func TestNodeChecksProveTheTimeDaemon(t *testing.T) {
	t.Parallel()
	want := [][]string{preset("enable " + timeDaemonUnit), active(timeDaemon), {"sh", "-c", "chronyc tracking > /dev/null"}, {"sh", "-c", chronyDropIns}}
	for _, b := range []Bake{
		{Distribution: Kubeadm, KubernetesVersion: "v1.37.2"}, {Distribution: RKE2}, {Distribution: K3s},
	} {
		checks := nodeChecks(b)
		for _, w := range want {
			if !slices.ContainsFunc(checks, func(c []string) bool { return slices.Equal(c, w) }) {
				t.Errorf("%s: no check %q among %q", b.Distribution, w, checks)
			}
		}
	}
}
