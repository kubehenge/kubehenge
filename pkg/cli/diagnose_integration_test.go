//go:build integration

package cli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/test/integration"
)

// bundleFiles lists the names in a support bundle.
func bundleFiles(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// TestIntegrationDiagnoseWritesANewBundle runs diagnose against an API server through KUBECONFIG:
// it writes the cluster's bundle to the file named and prints its name, refuses a file that is
// there already and leaves it as it was, and leaves no file behind for a cluster that is not there.
func TestIntegrationDiagnoseWritesANewBundle(t *testing.T) { //nolint:paralleltest // sets KUBECONFIG
	api := integration.Start(t, &integration.Options{Namespaces: []string{"team-a", integration.ClassNamespace}})
	c := &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{Paused: new(true)}}
	c.Namespace, c.Name = "team-a", "web"
	if err := api.Client.Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	useKubeconfig(t, api.Config)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "web.tar.gz")

	out, err := run(t, "diagnose", "team-a/web", "-o", bundle)
	if err != nil || out != bundle+"\n" {
		t.Fatalf("diagnose: %v\n%s", err, out)
	}
	if names := bundleFiles(t, bundle); !slices.Contains(names, "team-a-web/objects/cluster.x-k8s.io/Cluster/web.yaml") {
		t.Errorf("the bundle holds %q", names)
	}
	info, err := os.Stat(bundle)
	if err != nil {
		t.Fatal(err)
	}
	// Windows keeps no Unix modes.
	if runtime.GOOS != "windows" && info.Mode().Perm() != bundleMode {
		t.Errorf("the bundle's mode is %v", info.Mode().Perm())
	}

	if _, err = run(t, "diagnose", "team-a/web", "-o", bundle); err == nil || !strings.Contains(err.Error(), "--output") {
		t.Errorf("a second bundle to the same file: %v", err)
	}
	if again, statErr := os.Stat(bundle); statErr != nil || again.Size() != info.Size() {
		t.Errorf("the first bundle changed: %v", statErr)
	}

	missing := filepath.Join(dir, "gone.tar.gz")
	if _, err = run(t, "diagnose", "team-a/gone", "-o", missing); err == nil || !strings.Contains(err.Error(), "team-a/gone") {
		t.Errorf("a bundle of a cluster not there: %v", err)
	}
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed bundle left its file: %v", err)
	}
}
