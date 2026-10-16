package appliance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/release"
)

// archive builds a release archive whose files say which version they are.
func archive(t *testing.T, version string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"./bin/kubehenge": "binary " + version,
		"./share/kubehenge/management-cluster/main.tf":                 "module " + version,
		"./share/kubehenge/charts/kubehenge/Chart.yaml":                "chart " + version,
		"./share/kubehenge/appliance/kubehenge.service":                "unit " + version,
		"./share/kubehenge/appliance/kubehenge-update.service":         "updater " + version,
		"./share/kubehenge/appliance/kubehenge-update.path":            "path " + version,
		"./share/kubehenge/appliance/kubehenge-headlamp.service":       "headlamp " + version,
		"./share/kubehenge/appliance/kubehenge-headlamp.socket":        "socket " + version,
		"./share/kubehenge/appliance/kubehenge-headlamp-proxy.service": "proxy " + version,
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range extra {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tofuTarball is laid out as OpenTofu's own release archives are.
func tofuTarball(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range map[string]string{"LICENSE": "MPL", "tofu": body} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type releaseServer struct {
	url   string
	http  *http.Client
	key   ed25519.PublicKey
	files map[string][]byte
}

// newReleaseServer publishes 0.1.0 to 0.3.0 with a signed index; the 0.2.5 archive does not
// match its checksum.
func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	rs := &releaseServer{key: pub, files: map[string][]byte{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := rs.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	rs.url, rs.http = srv.URL, srv.Client()
	tofu := tofuTarball(t, "tofu 1.14.0")
	tofuSum := sha256.Sum256(tofu)
	rs.files["/tofu.tar.gz"] = tofu
	var ix release.Index
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.5", "0.3.0"} {
		a := archive(t, v)
		sum := sha256.Sum256(a)
		path := "/kubehenge_" + v + ".tar.gz"
		rs.files[path] = a
		if v == "0.2.5" {
			rs.files[path] = archive(t, "tampered")
		}
		r := release.Release{Version: v, URL: srv.URL + path, SHA256: hex.EncodeToString(sum[:])}
		if v == "0.2.0" {
			r.Tofu = &release.Artefact{Version: "1.14.0", URL: srv.URL + "/tofu.tar.gz", SHA256: hex.EncodeToString(tofuSum[:])}
		}
		ix.Releases = append(ix.Releases, r)
	}
	raw, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	rs.files["/index.json"] = raw
	rs.files["/index.json.sig"] = []byte(release.Sign(raw, priv))
	return rs
}

func newUpdater(t *testing.T, rs *releaseServer) (*Updater, *int) {
	t.Helper()
	root, spool := t.TempDir(), t.TempDir()
	for _, d := range []string{"usr/local/bin", "usr/local/share/kubehenge/management-cluster", "etc/systemd/system"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "usr", "local", "bin", "kubehenge"), []byte("binary 0.1.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(spool, "requests"), 0o700); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	return &Updater{
		Spool: Spool(spool), Index: rs.url + "/index.json", Root: root, HTTP: rs.http,
		Current: "0.1.0", Key: rs.key, ApplianceDir: t.TempDir(),
		Restart: func(context.Context) error { restarts++; return nil },
	}, &restarts
}

func TestUpdateWaitsForTheRunToEnd(t *testing.T) {
	t.Parallel()
	rs := newReleaseServer(t)
	u, restarts := newUpdater(t, rs)
	running, err := LockJobs(u.ApplianceDir)
	if err != nil {
		t.Fatal(err)
	}
	err = u.Spool.Request(UpdateRequest{Version: "0.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	err = u.Run(context.Background())
	if !errors.Is(err, ErrBusy) || *restarts != 0 {
		t.Fatalf("update during a run: %v, restarts %d", err, *restarts)
	}
	if got := read(t, filepath.Join(u.Root, "usr", "local", "bin", "kubehenge")); got != "binary 0.1.0" {
		t.Fatalf("binary replaced during a run: %q", got)
	}
	running()
	err = u.Spool.Request(UpdateRequest{Version: "0.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	err = u.Run(context.Background())
	if err != nil || *restarts != 1 {
		t.Fatalf("update after the run: %v, restarts %d", err, *restarts)
	}
	// Released again, so the restarted interface can start runs.
	unlock, err := LockJobs(u.ApplianceDir)
	if err != nil {
		t.Fatalf("lock left held: %v", err)
	}
	unlock()
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own files
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateToAVersion(t *testing.T) {
	t.Parallel()
	rs := newReleaseServer(t)
	u, restarts := newUpdater(t, rs)
	if err := u.Spool.Request(UpdateRequest{Version: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	if err := u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	bin, share := filepath.Join(u.Root, "usr", "local", "bin"), filepath.Join(u.Root, "usr", "local", "share")
	if got := read(t, filepath.Join(bin, "kubehenge")); got != "binary 0.2.0" {
		t.Fatalf("binary = %q", got)
	}
	if got := read(t, filepath.Join(share, "kubehenge", "management-cluster", "main.tf")); got != "module 0.2.0" {
		t.Fatalf("module = %q", got)
	}
	if got := read(t, filepath.Join(bin, "tofu")); got != "tofu 1.14.0" {
		t.Fatalf("tofu = %q", got)
	}
	if st, err := os.Stat(filepath.Join(bin, "tofu")); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o755 {
		t.Fatalf("tofu: %v, %v", st, err)
	}
	if left, _ := filepath.Glob(filepath.Join(share, ".kubehenge-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	for _, unit := range Units {
		if _, err := os.Stat(filepath.Join(u.Root, "etc", "systemd", "system", unit)); err != nil {
			t.Fatalf("unit %s: %v", unit, err)
		}
	}
	if st := u.Spool.Status(); st == nil || st.State != "succeeded" || st.From != "0.1.0" || st.To != "0.2.0" || *restarts != 1 {
		t.Fatalf("status = %+v, restarts %d", st, *restarts)
	}
	if _, err := os.Stat(filepath.Join(string(u.Spool), "requests", "upgrade.json")); !os.IsNotExist(err) {
		t.Fatal("the request was left in the spool")
	}
}

func TestUpdateByPolicy(t *testing.T) {
	t.Parallel()
	rs := newReleaseServer(t)
	u, _ := newUpdater(t, rs)
	u.Current = "0.3.0"
	if err := u.Spool.Request(UpdateRequest{Policy: release.PolicyMinor}); err != nil {
		t.Fatal(err)
	}
	if err := u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := u.Spool.Status(); st == nil || st.State != "current" {
		t.Fatalf("status = %+v", st)
	}
	if got := read(t, filepath.Join(u.Root, "usr", "local", "bin", "kubehenge")); got != "binary 0.1.0" {
		t.Fatalf("binary changed to %q with nothing newer", got)
	}
}

func TestUpdateRefuses(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		current string
		req     UpdateRequest
		tamper  func(*releaseServer)
	}{
		"older":           {current: "0.2.0", req: UpdateRequest{Version: "0.1.0"}},
		"same":            {current: "0.2.0", req: UpdateRequest{Version: "0.2.0"}},
		"wrong checksum":  {current: "0.1.0", req: UpdateRequest{Version: "0.2.5"}},
		"unsigned index":  {current: "0.1.0", req: UpdateRequest{Version: "0.2.0"}, tamper: func(rs *releaseServer) { rs.files["/index.json.sig"] = []byte("AAAA") }},
		"unknown version": {current: "0.1.0", req: UpdateRequest{Version: "0.9.0"}},
		"dev build":       {current: "dev", req: UpdateRequest{Version: "0.2.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rs := newReleaseServer(t)
			if c.tamper != nil {
				c.tamper(rs)
			}
			u, restarts := newUpdater(t, rs)
			u.Current = c.current
			if err := u.Spool.Request(c.req); err != nil {
				t.Fatal(err)
			}
			if err := u.Run(context.Background()); err == nil {
				t.Fatal("the upgrade went through")
			}
			if st := u.Spool.Status(); st == nil || st.State != "failed" || st.Error == "" {
				t.Fatalf("status = %+v", st)
			}
			if got := read(t, filepath.Join(u.Root, "usr", "local", "bin", "kubehenge")); got != "binary 0.1.0" || *restarts != 0 {
				t.Fatalf("binary = %q, restarts %d", got, *restarts)
			}
		})
	}
}

func TestUpdateRefusesBadRequests(t *testing.T) {
	t.Parallel()
	rs := newReleaseServer(t)
	for _, body := range []string{`{"version":"0.2.0","policy":"minor"}`, `{"policy":"off"}`, `{}`, `{"version":"0.2.0","url":"https://evil"}`, `not json`} {
		u, _ := newUpdater(t, rs)
		if err := os.WriteFile(filepath.Join(string(u.Spool), "requests", "upgrade.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := u.Run(context.Background()); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	u, _ := newUpdater(t, rs)
	if err := u.Run(context.Background()); err != nil || u.Spool.Status() != nil {
		t.Fatalf("an empty spool: %v, status %+v", err, u.Spool.Status())
	}
}

func TestUpdateRefusesASymlinkedRequest(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the updater runs on Linux")
	}
	rs := newReleaseServer(t)
	u, _ := newUpdater(t, rs)
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte(`{"version":"0.2.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(string(u.Spool), "requests", "upgrade.json")); err != nil {
		t.Fatal(err)
	}
	if err := u.Run(context.Background()); err == nil {
		t.Fatal("followed a symbolic link")
	}
}

func TestUnpackStaysInside(t *testing.T) {
	t.Parallel()
	for _, h := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg, Size: 0},
		{Name: "/etc/passwd", Typeflag: tar.TypeReg, Size: 0},
		{Name: "./share/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
	} {
		file := filepath.Join(t.TempDir(), "archive.tar.gz")
		if err := os.WriteFile(file, archive(t, "x", h), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := unpack(file, t.TempDir()); err == nil {
			t.Errorf("unpacked %s", h.Name)
		}
	}
}
