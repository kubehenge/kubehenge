package templates

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// TalosDefaultKubernetes is the Kubernetes release DefaultTalosVersion's Talos runs by default,
// which Kubehenge offers Talos clusters as the stable one.
var TalosDefaultKubernetes = "v" + constants.DefaultKubernetesVersion

// TalosMinor is a Talos release's minor version, such as 14 for v1.14.4.
func TalosMinor(version string) (int, error) {
	if !versionRE.MatchString(version) {
		return 0, fmt.Errorf("talos version %q is not of the form v1.2.3", version)
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if parts[0] != "1" {
		return 0, fmt.Errorf("talos version %q is not a 1.x release", version)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("talos version %q: %w", version, err)
	}
	return minor, nil
}

func talosRelease(version string) (*compatibility.TalosVersion, error) {
	if _, err := TalosMinor(version); err != nil {
		return nil, err
	}
	v, err := compatibility.ParseTalosVersion(&machine.VersionInfo{Tag: version})
	if err != nil {
		return nil, fmt.Errorf("talos version %q: %w", version, err)
	}
	return v, nil
}

// TalosRunsKubernetes tells whether a Talos release runs a Kubernetes release, by Talos's own
// compatibility table.
func TalosRunsKubernetes(talos, kubernetes string) error {
	t, err := talosRelease(talos)
	if err != nil {
		return err
	}
	if !versionRE.MatchString(kubernetes) {
		return fmt.Errorf("kubernetes version %q is not of the form v1.2.3", kubernetes)
	}
	k, err := compatibility.ParseKubernetesVersion(strings.TrimPrefix(kubernetes, "v"))
	if err != nil {
		return fmt.Errorf("kubernetes version %q: %w", kubernetes, err)
	}
	if err := k.SupportedWith(t); err != nil {
		return fmt.Errorf("talos %s does not run kubernetes %s: %w", talos, kubernetes, err)
	}
	return nil
}

// TalosKubernetesMinors are the Kubernetes minor versions a Talos release runs, oldest first.
func TalosKubernetesMinors(talos string) ([]int, error) {
	if _, err := talosRelease(talos); err != nil {
		return nil, err
	}
	var out []int
	// Kubernetes 1.x minors well past any release; Talos's table names a few dozen at most.
	for m := 1; m < 100; m++ {
		if TalosRunsKubernetes(talos, fmt.Sprintf("v1.%d.0", m)) == nil {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("talos %s runs no Kubernetes release Kubehenge knows", talos)
	}
	return out, nil
}

// TalosStep is the release a node on Talos from moves to next on its way to Talos to: to itself
// when it is at most one minor version on, else the newest of releases of the next minor version.
func TalosStep(from, to string, releases []string) (string, error) {
	was, err := TalosMinor(from)
	if err != nil {
		return "", err
	}
	now, err := TalosMinor(to)
	if err != nil {
		return "", err
	}
	if now <= was+1 {
		return to, nil
	}
	step, stepPatch := "", -1
	for _, r := range releases {
		minor, err := TalosMinor(r)
		if err != nil || minor != was+1 {
			continue
		}
		patch, err := strconv.Atoi(r[strings.LastIndex(r, ".")+1:])
		if err == nil && patch > stepPatch {
			step, stepPatch = r, patch
		}
	}
	if step == "" {
		return "", fmt.Errorf("no Talos 1.%d release to step through from %s to %s", was+1, from, to)
	}
	return step, nil
}

// TalosUpgrade tells whether nodes on Talos from may move to Talos to: never to an older minor
// version, and at most one minor version on, the path Talos tests its upgrades along.
func TalosUpgrade(from, to string) error {
	was, err := TalosMinor(from)
	if err != nil {
		return err
	}
	now, err := TalosMinor(to)
	if err != nil {
		return err
	}
	switch {
	case now < was:
		return fmt.Errorf("talos cannot go back from %s to %s", from, to)
	case now > was+1:
		return fmt.Errorf("talos upgrades one minor version at a time: from %s to the newest 1.%d release first", from, was+1)
	}
	host, err := talosRelease(from)
	if err != nil {
		return err
	}
	target, err := talosRelease(to)
	if err != nil {
		return err
	}
	if err := target.UpgradeableFrom(host); err != nil {
		return fmt.Errorf("talos %s cannot upgrade to %s: %w", from, to, err)
	}
	return nil
}
