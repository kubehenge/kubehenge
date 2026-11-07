package provisioner

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// patchRefresh is a refresh with a kubeadm cluster on v1.36.5, whose Kubernetes markers name the
// given releases.
func patchRefresh(t *testing.T, markers map[int]string, failing ...int) (*TemplateRefresh, *fakePool, *[]string) {
	t.Helper()
	pool := &fakePool{}
	var planned []string
	r := &TemplateRefresh{
		Kube: refreshKube(t, kubeadmCluster("api", "v1.36.5", day(20))), Pool: pool, ClassNamespace: "kubehenge-system",
		Keep: DefaultBuildsKept, Soak: DefaultNodeImageSoak, Now: func() time.Time { return day(22) },
		Planned: func(lines []templates.Template) {
			for _, l := range lines {
				planned = append(planned, l.LineName)
			}
		},
		Patches: func(_ context.Context, minor int) (string, bool, error) {
			if slices.Contains(failing, minor) {
				return "", false, errors.New("dl.k8s.io is down")
			}
			v, found := markers[minor]
			return v, found, nil
		},
	}
	return r, pool, &planned
}

func TestTemplateRefreshBuildsTheNewestKubeadmPatchAndTheNextMinor(t *testing.T) {
	t.Parallel()
	r, pool, planned := patchRefresh(t, map[int]string{36: "v1.36.7", 37: "v1.37.2"})
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kubehenge-rke2-almalinux-10", "kubehenge-kubeadm-v1.36.5-almalinux-10",
		"kubehenge-kubeadm-v1.36.7-almalinux-10", "kubehenge-kubeadm-v1.37.2-almalinux-10",
	}
	if !slices.Equal(*planned, want) || len(pool.built) != len(want) {
		t.Errorf("planned %v, built %v, want %v", *planned, pool.built, want)
	}
}

func TestTemplateRefreshBuildsNoPatchTheMarkersDoNotName(t *testing.T) {
	t.Parallel()
	// The next minor version is not released yet, and the running one's newest is the running one.
	r, _, planned := patchRefresh(t, map[int]string{36: "v1.36.5"})
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"kubehenge-rke2-almalinux-10", "kubehenge-kubeadm-v1.36.5-almalinux-10"}
	if !slices.Equal(*planned, want) {
		t.Errorf("planned %v, want %v", *planned, want)
	}
}

func TestTemplateRefreshBuildsNoKubeadmPatchesWhenTheSettingIsOff(t *testing.T) {
	t.Parallel()
	r, _, planned := patchRefresh(t, map[int]string{36: "v1.36.7", 37: "v1.37.2"})
	r.Patches = nil
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"kubehenge-rke2-almalinux-10", "kubehenge-kubeadm-v1.36.5-almalinux-10"}
	if !slices.Equal(*planned, want) {
		t.Errorf("planned %v, want %v", *planned, want)
	}
}

func TestTemplateRefreshLeavesAKubeadmPatchItAlreadyBuiltAlone(t *testing.T) {
	t.Parallel()
	r, pool, planned := patchRefresh(t, map[int]string{36: "v1.36.7"})
	pool.add(t, &templates.Request{Distribution: templates.Kubeadm, KubernetesVersion: "v1.36.7"}, day(15))
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(*planned, "kubehenge-kubeadm-v1.36.7-almalinux-10") {
		t.Errorf("planned %v, which builds v1.36.7 again", *planned)
	}
}

func TestTemplateRefreshReportsAMarkerItCannotReadAndGoesOn(t *testing.T) {
	t.Parallel()
	r, pool, planned := patchRefresh(t, map[int]string{36: "v1.36.7", 37: "v1.37.2"}, 37)
	err := r.Run(context.Background())
	if err == nil || !slices.Contains(*planned, "kubehenge-kubeadm-v1.36.7-almalinux-10") || slices.Contains(*planned, "kubehenge-kubeadm-v1.37.2-almalinux-10") {
		t.Errorf("run gave %v after planning %v", err, *planned)
	}
	if len(pool.built) != 3 {
		t.Errorf("built %v", pool.built)
	}
}

func TestTemplateRefreshMissingOnlyBuildsNoPatches(t *testing.T) {
	t.Parallel()
	r, pool, _ := patchRefresh(t, map[int]string{36: "v1.36.7"})
	r.MissingOnly = true
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pool.built) != 0 {
		t.Errorf("built %v", pool.built)
	}
}
