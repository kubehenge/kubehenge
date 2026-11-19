package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

const fakeBinary = "#!/bin/sh\necho \"kubehenge-ran $*\"\n"

// scriptRun is install.sh run in a scratch folder with a release whose binary only echoes its
// arguments, a curl that always fails, and no pct.
type scriptRun struct {
	sh, script, archive, sum, cache string
	env                             []string
}

func newScriptRun(t *testing.T) *scriptRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh runs in sh on the node")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	if err = tw.WriteHeader(&tar.Header{Name: "bin/kubehenge", Mode: 0o755, Size: int64(len(fakeBinary))}); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write([]byte(fakeBinary)); err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw.Bytes())
	r := &scriptRun{sh: sh, sum: hex.EncodeToString(sum[:]), cache: filepath.Join(dir, "cache")}
	r.archive = filepath.Join(dir, "release.tar.gz")
	if err = os.WriteFile(r.archive, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "appliance", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	r.script = filepath.Join(dir, "install.sh")
	body := strings.Replace(string(src), "KUBEHENGE_SHA256=\n", "KUBEHENGE_SHA256="+r.sum+"\n", 1)
	if err = os.WriteFile(r.script, []byte(body), 0o600); err != nil { //nolint:gosec // into the test's own temporary folder
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err = os.MkdirAll(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nexit 6\n"), 0o700); err != nil { //nolint:gosec // a script the test runs.
		t.Fatal(err)
	}
	r.env = []string{"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + dir, "HOME=" + dir, "KUBEHENGE_CACHE_DIR=" + r.cache}
	return r
}

func (r *scriptRun) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), r.sh, append([]string{r.script}, args...)...) //nolint:gosec // the script under test.
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// asNode gives the script a root user and a pct, so it goes on to the download.
func (r *scriptRun) asNode(t *testing.T) {
	t.Helper()
	bin := filepath.Join(filepath.Dir(r.script), "bin")
	for name, body := range map[string]string{"id": "#!/bin/sh\necho 0\n", "pct": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { //nolint:gosec // a script the test runs.
			t.Fatal(err)
		}
	}
}

// --help lists the options as a non-root user on no node, with no network and no space to unpack
// a release.
func TestInstallScriptHelpNeedsNoNode(t *testing.T) {
	t.Parallel()
	r := newScriptRun(t)
	r.env = append(r.env, "TMPDIR=/nonexistent")
	out, err := r.run(t, "--help")
	if err != nil || !strings.Contains(out, "--proxmox-url string") || strings.Contains(out, "kubehenge-ran") || strings.Contains(out, "kubehenge:") {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if out, err = r.run(t, "--release", r.archive, "--sha256", r.sum); err == nil || !strings.Contains(out, "kubehenge:") {
		t.Fatalf("an install on a machine that is no node went on: %v\n%s", err, out)
	}
}

// The options install.sh lists are the ones `kubehenge install` takes, word for word.
func TestInstallScriptHelpListsTheFlags(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "..", "appliance", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, listed, ok := strings.Cut(strings.ReplaceAll(string(src), "\r\n", "\n"), "\nUsage: sh install.sh [options]\n\n")
	listed, _, found := strings.Cut(listed, "\nOPTIONS\n")
	if !ok || !found {
		t.Fatal("install.sh has no list of options")
	}
	listed, _, _ = strings.Cut(listed, "\n\n  -h, --help")
	f := pflag.NewFlagSet("install", pflag.ContinueOnError)
	var c Config
	BindFlags(f, &c)
	want := strings.TrimSuffix(FlagUsages(f), "\n")
	if listed != want {
		t.Fatalf("install.sh lists other options than the installer takes. Want:\n%s\ngot:\n%s", want, listed)
	}
	for group, flag := range map[string]string{"Install:\n": "--ip string", "\nUpgrade:\n": "--no-trusted-proxy", "\nCertificate:\n": "--no-name"} {
		_, rest, ok := strings.Cut(want, group)
		if section, _, _ := strings.Cut(rest, ":\n"); !ok || !strings.Contains(section, flag) {
			t.Errorf("the %q group does not list %s:\n%s", strings.TrimSpace(group), flag, want)
		}
	}
}

// A release that was downloaded before is used from the cache, and a corrupt copy is not.
func TestInstallScriptUsesTheCachedRelease(t *testing.T) {
	t.Parallel()
	r := newScriptRun(t)
	r.asNode(t)
	if err := os.MkdirAll(r.cache, 0o750); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(r.cache, "kubehenge_0.1.0_"+r.sum+".tar.gz")
	data, err := os.ReadFile(r.archive)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cached, data, 0o600); err != nil { //nolint:gosec // into the test's own temporary folder
		t.Fatal(err)
	}
	out, err := r.run(t, "--ip", "dhcp")
	if err != nil || !strings.Contains(out, "using the copy downloaded before") || !strings.Contains(out, "kubehenge-ran install --ip dhcp") {
		t.Fatalf("the cached release was not used: %v\n%s", err, out)
	}
	if err = os.WriteFile(cached, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err = r.run(t, "--ip", "dhcp"); err == nil || !strings.Contains(out, "could not download") {
		t.Fatalf("a damaged copy was used: %v\n%s", err, out)
	}
}
