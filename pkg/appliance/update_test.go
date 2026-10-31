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
	"strings"
	"testing"
	"time"

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
	priv  ed25519.PrivateKey
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
	ix := release.Index{Expires: time.Now().Add(release.Validity)}
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
	rs.priv = priv
	rs.publish(t, &ix)
	return rs
}

// publish signs ix and serves it as the index.
func (rs *releaseServer) publish(t *testing.T, ix *release.Index) {
	t.Helper()
	raw, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	rs.files["/index.json"] = raw
	rs.files["/index.json.sig"] = []byte(release.Sign(raw, rs.priv))
}

func newUpdater(t *testing.T, rs *releaseServer) (*Updater, *int) {
	t.Helper()
	root, spool := t.TempDir(), t.TempDir()
	for _, d := range []string{"usr/local/bin", "usr/local/share/kubehenge/management-cluster", "etc/systemd/system"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installed := map[string]string{
		"usr/local/bin/kubehenge":                               "binary 0.1.0",
		"usr/local/share/kubehenge/management-cluster/main.tf":  "module 0.1.0",
		"usr/local/share/kubehenge/appliance/kubehenge.service": "unit 0.1.0",
		"etc/systemd/system/kubehenge.service":                  "unit 0.1.0",
	}
	for name, body := range installed {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(spool, "requests"), 0o700); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	return &Updater{
		Spool: Spool(spool), Index: rs.url + "/index.json", Root: root, HTTP: rs.http,
		Current: "0.1.0", Keys: []ed25519.PublicKey{rs.key}, ApplianceDir: t.TempDir(),
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

// installedIs checks that the binary, module, OpenTofu and interface unit in place are version's.
func installedIs(t *testing.T, u *Updater, version, tofu string) {
	t.Helper()
	share := filepath.Join(u.Root, "usr", "local", "share")
	for path, want := range map[string]string{
		u.binary("kubehenge"): "binary " + version,
		filepath.Join(share, "kubehenge", "management-cluster", "main.tf"):     "module " + version,
		filepath.Join(u.Root, "etc", "systemd", "system", "kubehenge.service"): "unit " + version,
	} {
		if got := read(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(u.Root, "etc", "systemd", "system", "kubehenge-update.path")); version == "0.1.0" && !os.IsNotExist(err) {
		t.Errorf("a unit only the new release has is left: %v", err)
	}
	if _, err := os.Stat(u.binary("tofu")); tofu == "" && !os.IsNotExist(err) {
		t.Errorf("OpenTofu the old release did not have is left: %v", err)
	} else if tofu != "" && read(t, u.binary("tofu")) != tofu {
		t.Errorf("tofu = %q, want %q", read(t, u.binary("tofu")), tofu)
	}
	if left, _ := filepath.Glob(filepath.Join(share, ".kubehenge-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// An upgrade whose interface does not serve is undone, and the status says so.
func TestUpdateRollsBack(t *testing.T) {
	t.Parallel()
	rs := newReleaseServer(t)
	u, restarts := newUpdater(t, rs)
	serving := func() string { return read(t, u.binary("kubehenge")) }
	u.Healthy = func(context.Context) error {
		if serving() == "binary 0.2.0" {
			return errors.New("the interface did not answer within 2m0s")
		}
		return nil
	}
	if err := u.Spool.Request(UpdateRequest{Version: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	err := u.Run(context.Background())
	want := "release 0.2.0 did not start: the interface did not answer within 2m0s; rolled back to 0.1.0"
	if err == nil || err.Error() != want || *restarts != 2 {
		t.Fatalf("update: %v, restarts %d", err, *restarts)
	}
	installedIs(t, u, "0.1.0", "")
	if st := u.Spool.Status(); st == nil || st.State != "failed" || st.From != "0.1.0" || st.To != "0.2.0" || st.Error != want || !st.RolledBack {
		t.Fatalf("status = %+v", st)
	}
	// Released again, so the interface can start runs.
	unlock, err := LockJobs(u.ApplianceDir)
	if err != nil {
		t.Fatalf("lock left held: %v", err)
	}
	unlock()

	// One that serves stays, and leaves nothing set aside.
	if err := u.Spool.Request(UpdateRequest{Version: "0.3.0"}); err != nil {
		t.Fatal(err)
	}
	if err := u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	installedIs(t, u, "0.3.0", "")
}

func TestUpdateRollsBackAFailedRestart(t *testing.T) {
	t.Parallel()
	rs := newReleaseServer(t)
	u, _ := newUpdater(t, rs)
	if err := os.WriteFile(u.binary("tofu"), []byte("tofu 1.13.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	u.Restart = func(context.Context) error {
		restarts++
		if restarts == 1 {
			return errors.New("systemctl restart kubehenge.service: exit status 1")
		}
		return nil
	}
	if err := u.Spool.Request(UpdateRequest{Version: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	err := u.Run(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), "; rolled back to 0.1.0") || restarts != 2 {
		t.Fatalf("update: %v, restarts %d", err, restarts)
	}
	installedIs(t, u, "0.1.0", "tofu 1.13.0")
}

// When the old release does not serve either, the status says both.
func TestUpdateRollBackFails(t *testing.T) {
	t.Parallel()
	rs := newReleaseServer(t)
	u, _ := newUpdater(t, rs)
	u.Healthy = func(context.Context) error { return errors.New("the interface answered 502 Bad Gateway") }
	if err := u.Spool.Request(UpdateRequest{Version: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	err := u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "; rolling back to 0.1.0 failed too: the interface answered 502") {
		t.Fatalf("update: %v", err)
	}
	installedIs(t, u, "0.1.0", "")
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
		tamper  func(*testing.T, *releaseServer)
	}{
		"older":          {current: "0.2.0", req: UpdateRequest{Version: "0.1.0"}},
		"same":           {current: "0.2.0", req: UpdateRequest{Version: "0.2.0"}},
		"wrong checksum": {current: "0.1.0", req: UpdateRequest{Version: "0.2.5"}},
		"unsigned index": {current: "0.1.0", req: UpdateRequest{Version: "0.2.0"}, tamper: func(_ *testing.T, rs *releaseServer) {
			rs.files["/index.json.sig"] = []byte("AAAA")
		}},
		"unknown version": {current: "0.1.0", req: UpdateRequest{Version: "0.9.0"}},
		"dev build":       {current: "dev", req: UpdateRequest{Version: "0.2.0"}},
		"expired index": {current: "0.1.0", req: UpdateRequest{Version: "0.2.0"}, tamper: func(t *testing.T, rs *releaseServer) {
			t.Helper()
			var ix release.Index
			if err := json.Unmarshal(rs.files["/index.json"], &ix); err != nil {
				t.Fatal(err)
			}
			ix.Expires = time.Now().Add(-time.Minute)
			raw, err := ix.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			rs.files["/index.json"], rs.files["/index.json.sig"] = raw, []byte(release.Sign(raw, rs.priv))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rs := newReleaseServer(t)
			if c.tamper != nil {
				c.tamper(t, rs)
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
		{Name: "share/../../escape", Typeflag: tar.TypeReg, Size: 0},
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

func TestUnpackFollowsNoLinkOut(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the updater runs on Linux")
	}
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "share")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "archive.tar.gz")
	h := &tar.Header{Name: "./share/escape", Typeflag: tar.TypeReg, Size: 0}
	if err := os.WriteFile(file, archive(t, "x", h), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unpack(file, dir); err == nil {
		t.Error("unpacked through a link out of the directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape")); err == nil {
		t.Error("wrote outside the directory")
	}
}

// Whatever an archive's entry is called and whatever kind it is, unpacking writes nothing beside
// the directory it unpacks into.
func FuzzUnpackStaysInside(f *testing.F) {
	for _, s := range []struct {
		name string
		kind byte
	}{
		{"./bin/kubehenge", tar.TypeReg},
		{"./share/kubehenge/", tar.TypeDir},
		{"../escape", tar.TypeReg},
		{"share/../../escape", tar.TypeReg},
		{"/etc/escape", tar.TypeReg},
		{`..\escape`, tar.TypeReg},
		{"./share/link", tar.TypeSymlink},
	} {
		f.Add(s.name, s.kind, []byte("x"))
	}
	f.Fuzz(func(t *testing.T, name string, kind byte, body []byte) {
		if runtime.GOOS == "windows" {
			t.Skip("the updater runs on Linux")
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		h := &tar.Header{Name: name, Typeflag: kind, Mode: 0o755, Size: int64(len(body)), Linkname: "../.."}
		if kind != tar.TypeReg {
			h.Size = 0
		}
		if tw.WriteHeader(h) != nil {
			return
		}
		if _, err := tw.Write(body[:h.Size]); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "archive.tar.gz")
		if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		parent := t.TempDir()
		dir := filepath.Join(parent, "dir")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = unpack(file, dir)
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("unpacking %q (type %q) wrote beside its directory: %v", name, kind, entries)
		}
	})
}
