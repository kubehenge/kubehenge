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
		{ReleaseMirror{URL: "https://user:secret@mirror.example.org"}, "holds credentials"}, //nolint:gosec // a fake address
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
	const own = "-----BEGIN CERTIFICATE-----\nMIIC\n-----END CERTIFICATE-----\n"
	const address = "https://mirror.example.org/github"
	const site = "https://site.example.org/gh"
	for _, tc := range []struct {
		name       string
		management ReleaseMirror
		cluster    string
		args, ca   string
	}{
		{"neither", ReleaseMirror{}, "", releaseScriptPath + " k3s ", ""},
		{"the management cluster's", ReleaseMirror{URL: address}, "", releaseScriptPath + " --mirror " + address + " k3s ", ""},
		{"the management cluster's with an authority", ReleaseMirror{URL: address, CABundle: ca}, "", releaseScriptPath + " --mirror " + address + " --mirror-ca " + releaseMirrorCAPath + " k3s ", ca},
		{"the cluster's own", ReleaseMirror{}, `{"url":"` + site + `"}`, releaseScriptPath + " --mirror " + site + " k3s ", ""},
		{"the cluster's own with an authority", ReleaseMirror{}, `{"url":"` + site + `","caBundle":` + quoted(t, own) + `}`, releaseScriptPath + " --mirror " + site + " --mirror-ca " + releaseMirrorCAPath + " k3s ", own},
		// The cluster's own replaces the management cluster's whole, its authority too.
		{"both", ReleaseMirror{URL: address, CABundle: ca}, `{"url":"` + site + `"}`, releaseScriptPath + " --mirror " + site + " k3s ", ""},
		{"both with authorities", ReleaseMirror{URL: address, CABundle: ca}, `{"url":"` + site + `","caBundle":` + quoted(t, own) + `}`, releaseScriptPath + " --mirror " + site + " --mirror-ca " + releaseMirrorCAPath + " k3s ", own},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mirrored := *r
			mirrored.Mirror = func() ReleaseMirror { return tc.management }
			c := cluster()
			if tc.cluster != "" {
				c = withAddons(c, map[string]string{releaseMirrorVariable: tc.cluster})
			}
			for _, kind := range []string{"KThreesControlPlaneTemplate", "KThreesConfigTemplate"} {
				patch, err := mirrored.patch(t.Context(), releaseItem(t, kind, "v1.37.1+k3s1", "v1.37.1+k3s1"), c)
				if err != nil {
					t.Fatal(err)
				}
				spec := releaseTemplates[kind].spec
				ops := releaseOps(t, patch)
				commands, ok := ops[spec+"/preK3sCommands/-"].(string)
				if !ok || !strings.HasPrefix(commands, tc.args) {
					t.Errorf("%s: the command is %q, want it to start %q", kind, commands, tc.args)
				}
				if got := mirrorCAOf(t, ops, spec); got != tc.ca {
					t.Errorf("%s: the node gets the authority %q, want %q", kind, got, tc.ca)
				}
			}
		})
	}
}

// mirrorCAOf is the mirror authority a patch gives the node, empty for none. It comes with the
// release script in the same patch, so it is on disk before the download starts.
func mirrorCAOf(t *testing.T, ops map[string]any, spec string) string {
	t.Helper()
	files, ok := ops[spec+"/files/-+"].([]any)
	if !ok {
		t.Fatalf("the files after the script are %v", ops[spec+"/files/-+"])
	}
	for _, f := range files {
		if file, isFile := f.(map[string]any); isFile && file["path"] == releaseMirrorCAPath {
			content, isText := file["content"].(string)
			if !isText {
				t.Fatalf("the authority's file holds %v", file["content"])
			}
			return content
		}
	}
	return ""
}

func TestAChangedReleaseMirrorChangesTheNodesTemplates(t *testing.T) {
	t.Parallel()
	r, _ := newReleaseChecksums(t)
	r.Mirror = func() ReleaseMirror { return ReleaseMirror{URL: "https://mirror.example.org/github"} }
	patchOf := func(variable string) string {
		c := cluster()
		if variable != "" {
			c = withAddons(c, map[string]string{releaseMirrorVariable: variable})
		}
		patch, err := r.patch(t.Context(), releaseItem(t, "KThreesConfigTemplate", "v1.37.1+k3s1", "v1.37.1+k3s1"), c)
		if err != nil {
			t.Fatal(err)
		}
		return string(patch)
	}
	// Cluster API rolls a cluster's machines one at a time when its templates change, as it does
	// for the other site settings.
	seen := map[string]string{}
	for _, variable := range []string{"", `{"url":"https://a.example.org"}`, `{"url":"https://b.example.org"}`} {
		patch := patchOf(variable)
		if prior, ok := seen[patch]; ok {
			t.Errorf("the mirror %q gives the same templates as %q", variable, prior)
		}
		seen[patch] = variable
	}
	const same = `{"url":"https://a.example.org"}`
	if first, again := patchOf(same), patchOf(same); first != again {
		t.Error("the same mirror gives other templates, which would roll the nodes for nothing")
	}
}

func TestAClustersReleaseMirrorIsHeldLikeTheManagementOne(t *testing.T) {
	t.Parallel()
	now := time.Now()
	valid := testCertificate(t, "valid", now.Add(-time.Hour), now.Add(time.Hour))
	expired := testCertificate(t, "old", now.Add(-48*time.Hour), now.Add(-time.Hour))
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("key")}))
	for name, tc := range map[string]struct{ value, want string }{
		"unset":                     {},
		"an address":                {value: `{"url":"https://m.example.org/github"}`},
		"plain http":                {value: `{"url":"http://10.0.0.5:8944"}`},
		"an authority":              {value: `{"url":"https://m.example.org","caBundle":` + quoted(t, valid) + `}`},
		"a private key":             {value: `{"url":"https://m.example.org","caBundle":` + quoted(t, valid+key) + `}`, want: "private key"},
		"an expired authority":      {value: `{"url":"https://m.example.org","caBundle":` + quoted(t, expired) + `}`, want: "expired on"},
		"text after the authority":  {value: `{"url":"https://m.example.org","caBundle":` + quoted(t, valid+"EOF\n") + `}`, want: "text besides"},
		"too large an authority":    {value: `{"url":"https://m.example.org","caBundle":` + quoted(t, strings.Repeat(valid, maxNodePEMBytes/len(valid)+1)) + `}`, want: "at most 64 KiB"},
		"an authority for http":     {value: `{"url":"http://m.example.org","caBundle":` + quoted(t, valid) + `}`, want: "for an https:// mirror"},
		"credentials":               {value: `{"url":"https://user:secret@m.example.org"}`, want: "holds credentials"}, //nolint:gosec // a fake address
		"a query":                   {value: `{"url":"https://m.example.org/?a=b"}`, want: "a query"},
		"a trailing slash":          {value: `{"url":"https://m.example.org/"}`, want: "ends in /"},
		"ftp":                       {value: `{"url":"ftp://m.example.org"}`, want: "no http:// or https:// address"},
		"an authority alone":        {value: `{"caBundle":` + quoted(t, valid) + `}`, want: "needs its address"},
		"a variable of a bad shape": {value: `"https://m.example.org"`, want: "variable releaseMirror"},
	} {
		c := cluster()
		if tc.value != "" {
			c = withAddons(c, map[string]string{releaseMirrorVariable: tc.value})
		}
		errs := validateNodeSite(c, now)
		switch {
		case tc.want == "" && len(errs) > 0:
			t.Errorf("%s: %v", name, errs)
		case tc.want != "" && !strings.Contains(errs.ToAggregate().Error(), tc.want):
			t.Errorf("%s: %v lacks %q", name, errs, tc.want)
		case len(errs) > 0 && strings.Contains(errs.ToAggregate().Error(), "secret"):
			t.Errorf("%s: the error shows the address's password: %v", name, errs)
		}
	}
}

// mirrorOf serves the releases newReleaseChecksums pins on GitHub's paths beneath /github, as a
// mirror does, and counts the requests that reach GitHub's own addresses.
func mirrorOf(f *fakeReleases, github *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
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
}

func TestReleaseChecksumsReadThroughTheMirror(t *testing.T) {
	t.Parallel()
	r, f := newReleaseChecksums(t)
	var github atomic.Int32
	srv := httptest.NewServer(mirrorOf(f, &github))
	t.Cleanup(srv.Close)
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
	srv := httptest.NewServer(mirrorOf(f, &atomic.Int32{}))
	t.Cleanup(srv.Close)
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
	srv := httptest.NewTLSServer(mirrorOf(f, &atomic.Int32{}))
	t.Cleanup(srv.Close)
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
