package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestVersionsFromTheReleaseChannels(t *testing.T) {
	t.Parallel()
	var fetched atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"stable","latest":"v1.36.5+rke2r1"},{"id":"latest","latest":"v1.37.1+rke2r1"},` +
			`{"id":"v1.30","latest":"v1.30.14+rke2r4"},{"id":"v1.33","latest":"v1.33.13+rke2r3"},{"id":"v1.34","latest":"v1.34.12+rke2r1"},` +
			`{"id":"v1.35","latest":"v1.35.9+rke2r2"},{"id":"v1.36","latest":"v1.36.5+rke2r1"},{"id":"v1.37","latest":"v1.37.1+rke2r1"}]}`))
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t)
	h.server.Releases = &provisioner.ChannelServer{HTTP: srv.Client(), URLs: map[string]string{"rke2": srv.URL}}
	d := h.browser()
	d.signIn("dev@pve")
	for range 2 {
		rec := d.do("GET", "/api/versions/rke2", "")
		expect(t, rec, http.StatusOK)
		var got versionsView
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		// New clusters start only on the minors Kubernetes still patches; the older ones are
		// there for clusters already on them to upgrade from.
		if !slices.Equal(got.Versions, []string{"v1.37.1+rke2r1", "v1.36.5+rke2r1", "v1.35.9+rke2r2", "v1.34.12+rke2r1"}) ||
			!slices.Equal(got.Older, []string{"v1.33.13+rke2r3", "v1.30.14+rke2r4"}) || got.Stable != "v1.36.5+rke2r1" {
			t.Fatalf("versions = %s", rec.Body)
		}
	}
	if fetched.Load() != 1 {
		t.Errorf("channels fetched %d times; they are kept a while", fetched.Load())
	}
	// k3s's channels are not configured here: the version is typed in, and the page says why.
	var k3s versionsView
	_ = json.Unmarshal(d.do("GET", "/api/versions/k3s", "").Body.Bytes(), &k3s)
	if len(k3s.Versions) != 0 || k3s.Error == "" {
		t.Fatalf("k3s = %+v", k3s)
	}
	expect(t, d.do("GET", "/api/versions/talos", ""), http.StatusNotFound)
}

func TestKubeadmVersionsAreThoseWithATemplate(t *testing.T) {
	t.Parallel()
	kubeadmVersions := func(recorded *string) versionsView {
		t.Helper()
		class := &clusterv1.ClusterClass{Namespace: "kubehenge-system", Name: "kubehenge-kubeadm"}
		if recorded != nil {
			class.Annotations = map[string]string{provisioner.KubeadmVersionsAnnotation: *recorded}
		}
		h := newHarness(t, class)
		// No privilege on the template pool: the controller has read it.
		d := h.browser()
		d.signIn("dev@pve")
		var got versionsView
		if err := json.Unmarshal(d.do("GET", "/api/versions/kubeadm", "").Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	recorded := "v1.37.1,v1.36.5,v1.36.4"
	if got := kubeadmVersions(&recorded); !slices.Equal(got.Versions, []string{"v1.37.1", "v1.36.5"}) || got.Error != "" {
		t.Fatalf("versions = %+v", got)
	}
	none := ""
	if got := kubeadmVersions(&none); len(got.Versions) != 0 || !strings.Contains(got.Error, "Administration page") {
		t.Fatalf("without templates = %+v", got)
	}
	if got := kubeadmVersions(nil); len(got.Versions) != 0 || !strings.Contains(got.Error, "not looked at the templates yet") {
		t.Fatalf("before the controller looked = %+v", got)
	}
}
