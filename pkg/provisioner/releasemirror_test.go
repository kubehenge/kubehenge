package provisioner

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReleaseMirrorCheck(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: certificateBlock, Bytes: srv.Certificate().Raw}))
	now := time.Now()
	for _, tc := range []struct {
		mirror ReleaseMirror
		want   string
	}{
		{ReleaseMirror{}, ""},
		{ReleaseMirror{URL: "https://mirror.example.org"}, ""},
		{ReleaseMirror{URL: "http://10.0.0.5:8080/github"}, ""},
		{ReleaseMirror{URL: "https://[2001:db8::1]:8443/gh"}, ""},
		{ReleaseMirror{URL: "https://mirror.example.org/github", CABundle: ca}, ""},
		{ReleaseMirror{CABundle: ca}, "needs a release mirror"},
		{ReleaseMirror{URL: "mirror.example.org"}, "no http:// or https:// address"},
		{ReleaseMirror{URL: "ftp://mirror.example.org"}, "no http:// or https:// address"},
		{ReleaseMirror{URL: "https://user:secret@mirror.example.org"}, "holds credentials"},
		{ReleaseMirror{URL: "https://mirror.example.org/?a=b"}, "a query"},
		{ReleaseMirror{URL: "https://mirror.example.org/a;rm"}, "characters other than"},
		{ReleaseMirror{URL: "https://mirror.example.org/a$(id)"}, "characters other than"},
		{ReleaseMirror{URL: "https://mirror.example.org/"}, "ends in /"},
		{ReleaseMirror{URL: "http://mirror.example.org", CABundle: ca}, "for an https:// mirror"},
		{ReleaseMirror{URL: "https://mirror.example.org", CABundle: "not a certificate"}, "holds no PEM certificate"},
	} {
		err := tc.mirror.Check(now)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%+v: %v", tc.mirror.URL, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%+v: got %v, want an error with %q", tc.mirror.URL, err, tc.want)
		case err != nil && strings.Contains(err.Error(), "secret"):
			t.Errorf("the error shows the address's password: %v", err)
		}
	}
}

func TestReleasePatchesDownloadFromTheMirror(t *testing.T) {
	t.Parallel()
	r, _ := newReleaseChecksums(t)
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	for _, tc := range []struct {
		name   string
		mirror ReleaseMirror
		args   string
		caFile bool
	}{
		{"unset", ReleaseMirror{}, releaseScriptPath + " k3s ", false},
		{"set", ReleaseMirror{URL: "https://mirror.example.org/github"}, releaseScriptPath + " --mirror https://mirror.example.org/github k3s ", false},
		{"with an authority", ReleaseMirror{URL: "https://mirror.example.org/github", CABundle: ca},
			releaseScriptPath + " --mirror https://mirror.example.org/github --mirror-ca " + releaseMirrorCAPath + " k3s ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mirrored := *r
			mirrored.Mirror = func() ReleaseMirror { return tc.mirror }
			patch, err := mirrored.patch(t.Context(), releaseItem(t, "KThreesConfigTemplate", "v1.37.1+k3s1", "v1.37.1+k3s1"), cluster())
			if err != nil {
				t.Fatal(err)
			}
			ops := releaseOps(t, patch)
			commands, ok := ops[templateSpec+"/preK3sCommands/-"].(string)
			if !ok || !strings.HasPrefix(commands, tc.args) {
				t.Errorf("the command is %q, want it to start %q", commands, tc.args)
			}
			var found bool
			files, _ := ops[templateSpec+"/files/-+"].([]any)
			for _, f := range files {
				if file, isFile := f.(map[string]any); isFile && file["path"] == releaseMirrorCAPath {
					found = file["content"] == ca
				}
			}
			if found != tc.caFile {
				t.Errorf("the node gets the mirror's authority: %v, want %v (files %v)", found, tc.caFile, files)
			}
		})
	}
}

// mirrorOf serves the releases newReleaseChecksums pins on GitHub's paths beneath /github, as a
// mirror does, and counts the requests that reach GitHub's own addresses.
func mirrorOf(t *testing.T, f *fakeReleases, tlsServer bool) (srv *httptest.Server, github *atomic.Int32) {
	t.Helper()
	github = &atomic.Int32{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path, mirrored := strings.CutPrefix(req.URL.Path, "/github")
		if !mirrored {
			github.Add(1)
			http.Error(w, "GitHub is out of reach", http.StatusBadGateway)
			return
		}
		repository, rest, _ := strings.Cut(path, "/raw/")
		if rest != "" {
			path = repository + "/" + rest
		}
		req.URL.Path = path
		f.ServeHTTP(w, req)
	})
	if tlsServer {
		srv = httptest.NewTLSServer(handler)
	} else {
		srv = httptest.NewServer(handler)
	}
	t.Cleanup(srv.Close)
	return srv, github
}

func TestReleaseChecksumsReadThroughTheMirror(t *testing.T) {
	t.Parallel()
	r, f := newReleaseChecksums(t)
	srv, github := mirrorOf(t, f, false)
	r.GitHub, r.Raw, r.HTTP = srv.URL, srv.URL, srv.Client()
	r.Mirror = func() ReleaseMirror { return ReleaseMirror{URL: srv.URL + "/github"} }
	pin, err := r.Digests(t.Context(), "v1.37.1+k3s1")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.pin(k3sRelease, "v1.37.1+k3s1"); pin != want {
		t.Errorf("read %+v through the mirror, want %+v", pin, want)
	}
	if n := github.Load(); n != 0 {
		t.Errorf("%d reads went to GitHub", n)
	}
}

func TestReleaseChecksumsNameTheMirrorAndTheFileItLacks(t *testing.T) {
	t.Parallel()
	r, f := newReleaseChecksums(t)
	srv, _ := mirrorOf(t, f, false)
	r.HTTP = srv.Client()
	mirror := srv.URL + "/github"
	r.Mirror = func() ReleaseMirror { return ReleaseMirror{URL: mirror} }
	f.mu.Lock()
	delete(f.files, "/"+k3sRelease.repository+"/v1.37.1+k3s1/install.sh")
	f.mu.Unlock()
	_, err := r.Digests(t.Context(), "v1.37.1+k3s1")
	if err == nil || !strings.Contains(err.Error(), "the release mirror "+mirror+" has no install.sh of k3s-io/k3s v1.37.1+k3s1") {
		t.Errorf("got %v, want the mirror and the file named", err)
	}
	if !errors.Is(err, errUnlisted) {
		t.Errorf("%v is not reported as a file that is not there", err)
	}
}

func TestReleaseChecksumsTrustTheMirrorsAuthority(t *testing.T) {
	t.Parallel()
	r, f := newReleaseChecksums(t)
	srv, _ := mirrorOf(t, f, true)
	mirror := ReleaseMirror{
		URL:      srv.URL + "/github",
		CABundle: string(pem.EncodeToMemory(&pem.Block{Type: certificateBlock, Bytes: srv.Certificate().Raw})),
	}
	if err := mirror.Check(time.Now()); err != nil {
		t.Fatal(err)
	}
	r.Mirror = func() ReleaseMirror { return mirror }
	r.HTTP = &http.Client{Transport: &http.Transport{}}
	if _, err := r.Digests(t.Context(), "v1.37.1+k3s1"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("without the authority the read gave %v, want a certificate error", err)
	}
	transport := &http.Transport{}
	if err := mirror.Trust(transport); err != nil {
		t.Fatal(err)
	}
	r.HTTP = &http.Client{Transport: transport}
	pin, err := r.Digests(t.Context(), "v1.37.1+k3s1")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.pin(k3sRelease, "v1.37.1+k3s1"); pin != want {
		t.Errorf("read %+v through the mirror, want %+v", pin, want)
	}
}
