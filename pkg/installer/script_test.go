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
)

const fakeBinary = "#!/bin/sh\necho \"kubehenge-ran $*\"\n"

// scriptRun is install.sh run in a scratch folder with a release whose binary only echoes its
// arguments, a curl that always fails, and no pct.
type scriptRun struct {
	script, archive, sum, cache string
	env                         []string
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
	r := &scriptRun{sum: hex.EncodeToString(sum[:]), cache: filepath.Join(dir, "cache")}
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
	if err = os.WriteFile(r.script, []byte(body), 0o600); err != nil {
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
	_ = sh
	return r
}

func (r *scriptRun) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{r.script}, args...)...) //nolint:gosec // the script under test.
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --help is answered before the root and node checks, so it works from any shell.
func TestInstallScriptHelpNeedsNoNode(t *testing.T) {
	t.Parallel()
	r := newScriptRun(t)
	out, err := r.run(t, "--release", r.archive, "--sha256", r.sum, "--help")
	if err != nil || !strings.Contains(out, "kubehenge-ran install --release") || !strings.Contains(out, "--help") {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if out, err = r.run(t, "--release", r.archive, "--sha256", r.sum); err == nil || !strings.Contains(out, "kubehenge:") {
		t.Fatalf("an install on a machine that is no node went on: %v\n%s", err, out)
	}
}

// A release that was downloaded before is used from the cache, and a corrupt copy is not.
func TestInstallScriptUsesTheCachedRelease(t *testing.T) {
	t.Parallel()
	r := newScriptRun(t)
	if err := os.MkdirAll(r.cache, 0o750); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(r.cache, "kubehenge_0.1.0_"+r.sum+".tar.gz")
	data, err := os.ReadFile(r.archive)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cached, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := r.run(t, "--help")
	if err != nil || !strings.Contains(out, "using the copy downloaded before") || !strings.Contains(out, "kubehenge-ran install --help") {
		t.Fatalf("the cached release was not used: %v\n%s", err, out)
	}
	if err = os.WriteFile(cached, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err = r.run(t, "--help"); err == nil || !strings.Contains(out, "could not download") {
		t.Fatalf("a damaged copy was used: %v\n%s", err, out)
	}
}
