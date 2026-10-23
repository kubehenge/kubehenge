package provisioner

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// manifestServer serves files by path; the first request for /flaky fails.
func manifestServer(t *testing.T, files map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var (
		requests atomic.Int32
		failed   atomic.Bool
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/flaky" && !failed.Swap(true) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func manifestsClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestStoreProviderManifests(t *testing.T) {
	t.Parallel()
	const metadata, components = "apiVersion: clusterctl.cluster.x-k8s.io/v1alpha3\n", "kind: Namespace\n"
	srv, _ := manifestServer(t, map[string]string{"/metadata.yaml": metadata, "/flaky": components})
	older := &corev1.ConfigMap{
		Namespace: "capi-system", Name: "coreprovider-cluster-api-v1.14.2",
		Labels: map[string]string{ManifestsLabel: "coreprovider-cluster-api", manifestsVersionLabel: "v1.14.2"},
	}
	unrelated := &corev1.ConfigMap{
		Namespace: "capi-system", Name: "bootstrapprovider-kubeadm-v1.14.2",
		Labels: map[string]string{ManifestsLabel: "bootstrapprovider-kubeadm", manifestsVersionLabel: "v1.14.2"},
	}
	c := manifestsClient(t, older, unrelated)
	p := ProviderManifests{
		Kind: "CoreProvider", Name: "cluster-api", Namespace: "capi-system", Version: "v1.14.3",
		Metadata:   srv.URL + "/metadata.yaml@sha256:" + digestOf(metadata),
		Components: srv.URL + "/flaky@sha256:" + digestOf(components),
	}
	if err := StoreProviderManifests(t.Context(), c, srv.Client(), []ProviderManifests{p}, time.Millisecond); err != nil {
		t.Fatal(err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "capi-system", Name: "coreprovider-cluster-api-v1.14.3"}, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Labels[ManifestsLabel] != "coreprovider-cluster-api" || cm.Labels[manifestsVersionLabel] != "v1.14.3" {
		t.Errorf("labels = %v", cm.Labels)
	}
	if cm.Annotations["provider.cluster.x-k8s.io/compressed"] != "true" {
		t.Errorf("annotations = %v", cm.Annotations)
	}
	if cm.Data["metadata"] != metadata {
		t.Errorf("metadata = %q", cm.Data["metadata"])
	}
	zr, err := gzip.NewReader(bytes.NewReader(cm.BinaryData["components"]))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(zr); err != nil || string(got) != components {
		t.Errorf("components = %q, %v", got, err)
	}

	list := &corev1.ConfigMapList{}
	if err := c.List(t.Context(), list, client.InNamespace("capi-system")); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	if len(names) != 2 || names[0] != "bootstrapprovider-kubeadm-v1.14.2" || names[1] != "coreprovider-cluster-api-v1.14.3" {
		t.Errorf("ConfigMaps = %v, want the other provider's and the new version's", names)
	}
}

func TestStoreProviderManifestsRefusesAWrongDigest(t *testing.T) {
	t.Parallel()
	srv, requests := manifestServer(t, map[string]string{"/metadata.yaml": "changed"})
	p := ProviderManifests{
		Kind: "CoreProvider", Name: "cluster-api", Namespace: "capi-system", Version: "v1.14.3",
		Metadata:   srv.URL + "/metadata.yaml@sha256:" + digestOf("published"),
		Components: srv.URL + "/components.yaml@sha256:" + digestOf("published"),
	}
	err := StoreProviderManifests(t.Context(), manifestsClient(t), srv.Client(), []ProviderManifests{p}, time.Millisecond)
	if !errors.Is(err, errDigest) {
		t.Fatalf("err = %v, want a digest mismatch", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests, want one: a wrong file is not fetched again", n)
	}
}

type moduleSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type moduleFetchConfig struct {
	Selector moduleSelector `json:"selector"`
}

type moduleProviderSpec struct {
	Version     string            `json:"version"`
	FetchConfig moduleFetchConfig `json:"fetchConfig"`
}

type moduleProvider struct {
	Kind string             `json:"kind"`
	Name string             `json:"name"`
	Spec moduleProviderSpec `json:"spec"`
}

// The module writes each provider's selector itself; the hook must store under the same one.
func TestModuleSelectsTheStoredManifests(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/cluster-api-providers-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Providers []moduleProvider `json:"providers"`
	}
	if err = yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if len(values.Providers) == 0 {
		t.Fatal("no providers")
	}
	for _, p := range values.Providers {
		want := map[string]string{ManifestsLabel: ManifestsSelector(p.Kind, p.Name), manifestsVersionLabel: p.Spec.Version}
		if got := p.Spec.FetchConfig.Selector.MatchLabels; !maps.Equal(got, want) {
			t.Errorf("%s %s selects %v, but its manifests are stored under %v", p.Kind, p.Name, got, want)
		}
	}
}

func TestDownloadManifestNeedsHTTPSAndADigest(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"http://example.org/metadata.yaml@sha256:" + digestOf("x"),
		"https://example.org/metadata.yaml",
		"https://example.org/metadata.yaml@sha256:ABC",
	} {
		if _, err := downloadManifest(t.Context(), http.DefaultClient, ref, time.Millisecond); err == nil {
			t.Errorf("%s: no error", ref)
		}
	}
}
