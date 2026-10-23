package templates

import (
	"slices"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config"
)

func TestTalosContractIsTheReleasesMinor(t *testing.T) {
	t.Parallel()
	for release, want := range map[string]string{"v1.14.4": "v1.14", "v1.13.12": "v1.13", "v1.15.0": "v1.15"} {
		got, err := TalosContract(release)
		if err != nil || got != want {
			t.Errorf("TalosContract(%s) = %q, %v, want %s", release, got, err, want)
		}
	}
	// The default release's contract is one the machinery Kubehenge generates with knows.
	contract, err := TalosContract(DefaultTalosVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.ParseContractFromVersion(contract); err != nil {
		t.Errorf("contract %s: %v", contract, err)
	}
	for _, malformed := range []string{"1.14.4", "v2.0.0", "v1.14"} {
		if _, err := TalosContract(malformed); err == nil {
			t.Errorf("a contract for %q", malformed)
		}
	}
}

func TestTalosRunsTheKubernetesItsTableNames(t *testing.T) {
	t.Parallel()
	if err := TalosRunsKubernetes(DefaultTalosVersion, TalosDefaultKubernetes); err != nil {
		t.Errorf("the default release's own Kubernetes: %v", err)
	}
	for _, kubernetes := range []string{"v1.20.0", "v1.99.0"} {
		if TalosRunsKubernetes(DefaultTalosVersion, kubernetes) == nil {
			t.Errorf("Talos %s runs Kubernetes %s", DefaultTalosVersion, kubernetes)
		}
	}
	if TalosRunsKubernetes("1.14", TalosDefaultKubernetes) == nil || TalosRunsKubernetes(DefaultTalosVersion, "1.37") == nil {
		t.Error("malformed versions taken")
	}
}

func TestTalosKubernetesMinorsAreARunOfMinors(t *testing.T) {
	t.Parallel()
	minors, err := TalosKubernetesMinors(DefaultTalosVersion)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(minors); i++ {
		if minors[i] != minors[i-1]+1 {
			t.Fatalf("minors %v", minors)
		}
	}
	// Kubernetes versions read as Talos ones do: v1.<minor>.<patch>.
	stable, err := TalosMinor(TalosDefaultKubernetes)
	if err != nil || !slices.Contains(minors, stable) || len(minors) < 3 {
		t.Errorf("minors %v lack the default's 1.%d", minors, stable)
	}
	// A release past the tables Kubehenge is built with runs nothing it could check.
	if minors, err := TalosKubernetesMinors("v1.99.0"); err == nil {
		t.Errorf("Talos v1.99.0 runs %v", minors)
	}
}

func TestTalosUpgradesOneMinorAtATime(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"v1.14.1", "v1.14.4", true},
		{"v1.13.5", "v1.14.4", true},
		{"v1.12.6", "v1.14.4", false},
		{"v1.14.4", "v1.13.5", false},
		{"v1.14", "v1.14.4", false},
	} {
		if err := TalosUpgrade(c.from, c.to); (err == nil) != c.ok {
			t.Errorf("%s to %s: %v", c.from, c.to, err)
		}
	}
}

func TestTalosStepsThroughTheNewestOfEachMinor(t *testing.T) {
	t.Parallel()
	releases := []string{"v1.13.2", "v1.13.10", "v1.13.9", "v1.14.4", "v1.12.0"}
	for _, c := range []struct{ from, to, want string }{
		{"v1.14.1", "v1.14.4", "v1.14.4"},
		{"v1.13.0", "v1.14.4", "v1.14.4"},
		{"v1.12.3", "v1.14.4", "v1.13.10"},
	} {
		if got, err := TalosStep(c.from, c.to, releases); err != nil || got != c.want {
			t.Errorf("%s to %s: %q, %v", c.from, c.to, got, err)
		}
	}
	if _, err := TalosStep("v1.10.0", "v1.14.4", releases); err == nil {
		t.Error("stepped through a minor with no release")
	}
}
