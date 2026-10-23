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
