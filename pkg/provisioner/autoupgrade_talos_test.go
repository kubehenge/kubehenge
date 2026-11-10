package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestTalosChannelsAreKubernetesReleasesTalosRuns(t *testing.T) {
	t.Parallel()
	minors, err := templates.TalosKubernetesMinors(templates.DefaultTalosVersion)
	if err != nil {
		t.Fatal(err)
	}
	missing := minors[0]
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		var minor int
		if _, scanErr := fmt.Sscanf(r.URL.Path, "/stable-1.%d.txt", &minor); scanErr != nil || minor == missing {
			// A minor Talos runs that has no release file is left out, not an error.
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "v1.%d.7\n", minor)
	}))
	t.Cleanup(srv.Close)
	s := &ChannelServer{HTTP: srv.Client(), URLs: map[string]string{"talos": srv.URL}}
	ch, err := s.Channels(context.Background(), "talos", templates.NodeOS{})
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := parseKubeVersion(templates.TalosDefaultKubernetes)
	if ch.StableMinor != stable.Minor || len(ch.Latest) != len(minors)-1 || ch.Latest[minors[1]].Patch != 7 {
		t.Errorf("channels %+v", ch)
	}
	if _, ok := ch.Latest[missing]; ok || int(asked.Load()) != len(minors) {
		t.Errorf("asked %d times, channels %+v", asked.Load(), ch)
	}

	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v1.2.3"))
	}))
	t.Cleanup(wrong.Close)
	s = &ChannelServer{HTTP: wrong.Client(), URLs: map[string]string{"talos": wrong.URL}}
	if _, err := s.Channels(context.Background(), "talos", templates.NodeOS{}); err == nil || !strings.Contains(err.Error(), "names no Kubernetes") {
		t.Errorf("another minor's release taken: %v", err)
	}
}

func TestNewestTalosPatchStaysOnTheMinor(t *testing.T) {
	t.Parallel()
	releases := []string{"v1.13.12", "v1.14.2", "v1.14.10", "v1.14.9", "v1.15.0", "garbage"}
	for _, c := range []struct{ current, want string }{
		{"v1.14.1", "v1.14.10"},
		{"v1.14.10", "v1.14.10"},
		{"v1.14.11", "v1.14.11"},
		{"v1.13.3", "v1.13.12"},
		{"v1.12.0", "v1.12.0"},
		{"1.14", "1.14"},
	} {
		if got := newestTalosPatch(c.current, releases); got != c.want {
			t.Errorf("newestTalosPatch(%s) = %s, want %s", c.current, got, c.want)
		}
	}
}

// talosOn is a Talos cluster on the Talos release and Kubernetes version, with the upgrades policy.
func talosOn(talos, kubernetes, policy string) *clusterv1.Cluster {
	c := talosCluster("talos", talos)
	c.Spec.Topology.Version = kubernetes
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "upgrades"})
	c.Spec.Topology.Variables[len(c.Spec.Topology.Variables)-1].Value.Raw = []byte(policy)
	return c
}

// Talos v1.13 runs Kubernetes up to 1.36, v1.14 up to 1.37.
func TestTalosReleaseHoldOnTheNextMinorStillTakesPatchReleases(t *testing.T) {
	t.Parallel()
	releases := fixedReleases{"talos": channels(37, "v1.36.5", "v1.37.1")}
	for _, c := range []struct{ name, from, want, reason string }{
		{"a patch release is due", "v1.36.2", "v1.36.5", ""},
		{"the newest patch is running", "v1.36.5", "v1.36.5", "TalosRelease"},
	} {
		got := clusterAfter(t, upgradeRound(t, &Upgrader{Releases: releases}, talosOn("v1.13.0", c.from, `{"mode":"minor"}`)), "talos")
		if got.Spec.Topology.Version != c.want {
			t.Errorf("%s: Talos v1.13.0 does not run 1.37 and the cluster stays on %s, want %s", c.name, got.Spec.Topology.Version, c.want)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, AutoUpgradeSkippedCondition)
		switch {
		case c.reason == "" && cond != nil:
			t.Errorf("%s: the upgrade it took carries the note %+v", c.name, cond)
		case c.reason != "" && (cond == nil || cond.Reason != c.reason || !strings.Contains(cond.Message, "v1.37.1")):
			t.Errorf("%s: note %+v, want the reason %s and the version it holds back", c.name, cond, c.reason)
		}
	}
}

func TestUpgraderKeepsATalosClusterToTheKubernetesItsTalosReleaseRuns(t *testing.T) {
	t.Parallel()
	u := &Upgrader{Releases: fixedReleases{"talos": channels(37, "v1.36.5", "v1.37.1")}}
	if got := clusterAfter(t, upgradeRound(t, u, talosOn("v1.13.0", "v1.36.5", `{"mode":"minor"}`)), "talos").Spec.Topology.Version; got != "v1.36.5" {
		t.Errorf("Talos v1.13.0 runs no Kubernetes 1.37: the cluster moved to %s", got)
	}
	if got := clusterAfter(t, upgradeRound(t, &Upgrader{Releases: u.Releases}, talosOn("v1.14.0", "v1.36.5", `{"mode":"minor"}`)), "talos").Spec.Topology.Version; got != "v1.37.1" {
		t.Errorf("Talos v1.14.0 runs Kubernetes 1.37: the cluster stays on %s", got)
	}
}

func TestUpgraderWaitsForATalosNodeUpgradeBeforeAKubernetesUpgrade(t *testing.T) {
	t.Parallel()
	c := talosOn("v1.14.0", "v1.36.2", `{"mode":"minor"}`)
	c.Annotations[TalosUpgradingAnnotation] = "web-cp-1 to v1.14.4 since 2026-10-13T02:30:00Z"
	u := &Upgrader{Releases: fixedReleases{"talos": channels(37, "v1.36.5", "v1.37.1")}}
	if got := clusterAfter(t, upgradeRound(t, u, c), "talos").Spec.Topology.Version; got != "v1.36.2" {
		t.Errorf("a node is moving to another Talos release and the cluster moved to Kubernetes %s", got)
	}
}

func TestTalosPatchWaitsForAReleaseThatRunsTheClustersKubernetes(t *testing.T) {
	t.Parallel()
	// Kubernetes 1.38 is past what any Talos v1.14 release runs.
	u := &Upgrader{Releases: fixedReleases{}, TalosImages: fakeTalosImages{"v1.14.4"}}
	c := talosOn("v1.14.0", "v1.38.0", `{"mode":"off"}`)
	if got, err := TalosVersion(clusterAfter(t, upgradeRound(t, u, c), "talos")); err != nil || got != "v1.14.0" {
		t.Errorf("Talos %s, %v: moved to a release that does not run the cluster's Kubernetes", got, err)
	}
}

func TestTalosPatchWaitsForAClusterThatIsRolling(t *testing.T) {
	t.Parallel()
	two := int32(2)
	c := talosOn("v1.14.0", "v1.36.5", `{"mode":"off"}`)
	c.Status.ControlPlane.DesiredReplicas = &two
	u := &Upgrader{Releases: fixedReleases{}, TalosImages: fakeTalosImages{"v1.14.4"}}
	if got, err := TalosVersion(clusterAfter(t, upgradeRound(t, u, c), "talos")); err != nil || got != "v1.14.0" {
		t.Errorf("Talos %s, %v: patched while the cluster rolls", got, err)
	}
}

func TestUpgraderMovesTalosToTheNewestPatchInsideTheWindow(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := func(c *clusterv1.Cluster, p string) *clusterv1.Cluster {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "upgrades"})
		c.Spec.Topology.Variables[len(c.Spec.Topology.Variables)-1].Value.Raw = []byte(p)
		return c
	}
	failed := policy(talosCluster("failed", "v1.14.1"), `{"mode":"off"}`)
	failed.Annotations[TalosUpgradeFailedAnnotation] = "web-cp-1: not Ready"
	upgrading := policy(talosCluster("upgrading", "v1.14.1"), `{"mode":"off"}`)
	upgrading.Annotations[TalosUpgradingAnnotation] = "web-cp-1 to v1.14.4 since 2026-10-13T02:30:00Z"
	objects := []client.Object{
		policy(talosCluster("on", "v1.14.1"), `{"mode":"off"}`),
		policy(talosCluster("newest", "v1.14.4"), `{"mode":"off"}`),
		policy(talosCluster("no-images", "v1.14.1"), `{"mode":"off","nodeImages":false}`),
		policy(talosCluster("closed", "v1.14.1"), `{"mode":"off","window":{"weekdays":["Sun"]}}`),
		failed, upgrading,
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	for _, o := range objects {
		status, ok := o.DeepCopyObject().(client.Object)
		if !ok {
			t.Fatalf("%T copies to no object", o)
		}
		if err := kube.Status().Update(context.Background(), status); err != nil {
			t.Fatal(err)
		}
	}
	u := &Upgrader{
		Client:         kube,
		ClassNamespace: "kubehenge-system",
		Releases:       fixedReleases{},
		TalosImages:    fakeTalosImages{"v1.13.12", "v1.14.4", "v1.15.0"},
		Now:            func() time.Time { return time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC) },
	}
	u.RunOnce(context.Background())
	for name, want := range map[string]string{
		"on": "v1.14.4", "newest": "v1.14.4", "no-images": "v1.14.1", "closed": "v1.14.1",
		"failed": "v1.14.1", "upgrading": "v1.14.1",
	} {
		c := &clusterv1.Cluster{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: name}, c); err != nil {
			t.Fatal(err)
		}
		got, err := TalosVersion(c)
		if err != nil || got != want {
			t.Errorf("%s: Talos %s, %v; want %s", name, got, err, want)
		}
		if moved := c.Annotations[autoUpgraded] != ""; moved != (name == "on") {
			t.Errorf("%s: annotation %q", name, c.Annotations[autoUpgraded])
		}
	}
}
