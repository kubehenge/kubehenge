package appliance

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// providerPackage is a provider's package as the registry serves it: a zip of its binary.
func providerPackage(t *testing.T, binary string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.Create("terraform-provider-fake_v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte(binary)); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The appliance's OpenTofu, the real one, installs the module's providers from the release's own
// copies beside the module, checked against the module's lock file, and from nowhere else.
func TestInitInstallsTheProvidersTheReleaseCarries(t *testing.T) {
	t.Parallel()
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatal("this test runs OpenTofu, which make unit needs on PATH")
	}
	share := t.TempDir()
	module := filepath.Join(share, "management-cluster")
	mirrored := filepath.Join(share, "providers", "registry.opentofu.org", "kubehenge", "fake")
	for _, dir := range []string{module, mirrored} {
		if err = os.MkdirAll(dir, privateDir); err != nil {
			t.Fatal(err)
		}
	}
	platform := runtime.GOOS + "_" + runtime.GOARCH
	pkg := filepath.Join(mirrored, "terraform-provider-fake_1.0.0_"+platform+".zip")
	shipped := providerPackage(t, "the release's provider")
	if err = os.WriteFile(pkg, shipped, privateFile); err != nil {
		t.Fatal(err)
	}
	// setUp has the module require the providers and pin each to its package's checksum.
	a := &Appliance{Dir: t.TempDir(), Module: module, Tofu: tofu, Log: slog.New(slog.DiscardHandler)}
	setUp := func(pins map[string][]byte) {
		t.Helper()
		var lock, required strings.Builder
		for _, name := range slices.Sorted(maps.Keys(pins)) {
			fmt.Fprintf(&lock, "provider \"registry.opentofu.org/kubehenge/%s\" {\n  version = \"1.0.0\"\n  hashes = [\n    \"zh:%x\",\n  ]\n}\n", name, sha256.Sum256(pins[name]))
			fmt.Fprintf(&required, "    %s = { source = \"kubehenge/%s\" }\n", name, name)
		}
		if err := os.WriteFile(filepath.Join(module, ".terraform.lock.hcl"), []byte(lock.String()), privateFile); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(a.workdir()); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
			t.Fatal(err)
		}
		root := "terraform {\n  required_providers {\n" + required.String() + "  }\n}\n"
		if err := os.WriteFile(filepath.Join(a.workdir(), "main.tf"), []byte(root), privateFile); err != nil {
			t.Fatal(err)
		}
	}
	initialise := func() (*Job, error) {
		t.Helper()
		env, envErr := a.env(testPassphrase, nil)
		if envErr != nil {
			t.Fatal(envErr)
		}
		j := &Job{}
		return j, a.init(t.Context(), j, env)
	}

	setUp(map[string][]byte{"fake": shipped})
	if j, initErr := initialise(); initErr != nil {
		t.Fatalf("init from the release's providers: %v\n%s", initErr, strings.Join(j.Log(), "\n"))
	}
	installed := filepath.Join(a.workdir(), ".terraform", "providers", "registry.opentofu.org", "kubehenge", "fake", "1.0.0", platform, "terraform-provider-fake_v1.0.0")
	if got, readErr := os.ReadFile(installed); readErr != nil || string(got) != "the release's provider" { //nolint:gosec // the test's own file
		t.Errorf("the installed provider holds %q, %v", got, readErr)
	}

	setUp(map[string][]byte{"fake": providerPackage(t, "another build")})
	if j, initErr := initialise(); initErr == nil || !strings.Contains(strings.Join(j.Log(), "\n"), "doesn't match any of the checksums") {
		t.Errorf("init of a package the lock file does not pin = %v\n%s", initErr, strings.Join(j.Log(), "\n"))
	}

	setUp(map[string][]byte{"fake": shipped, "unshipped": providerPackage(t, "elsewhere")})
	if j, initErr := initialise(); initErr == nil || !strings.Contains(strings.Join(j.Log(), "\n"), "kubehenge/unshipped") {
		t.Errorf("init of a provider the release lacks = %v\n%s", initErr, strings.Join(j.Log(), "\n"))
	}
}
