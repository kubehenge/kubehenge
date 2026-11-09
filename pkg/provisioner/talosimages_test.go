package provisioner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// answerStatus is a scripted answer with that status and no body.
func answerStatus(code int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

// cutOff sends half of what it promises and drops the connection, so that the answer has begun and
// the transport cannot send the request again by itself.
func cutOff(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "1000")
	_, _ = w.Write([]byte(strings.Repeat("x", 500)))
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

type tarFile struct{ name, body string }

// gzipTar is a layer: a gzipped tar of files, with mtime in the gzip header.
func gzipTar(files []tarFile, mtime time.Time) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.ModTime = mtime
	tw := tar.NewWriter(gz)
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// releaseSpec is how a fake Sidero release differs from a good one.
type releaseSpec struct {
	docker         bool
	imagerLabel    string
	installerLabel string
	// layers replace the extensions image's one layer, and digests the content of its image-digests.
	layers  [][]tarFile
	digests string
	// skip is a repository with no tag for the release, bigConfig makes the imager's config over
	// the cap.
	skip      string
	bigConfig bool
	// platforms are the entries of the imager's index besides the amd64 one: "arm64", "attestation",
	// "no amd64" drops the amd64 one, "two amd64" adds a second.
	platforms []string
}

func goodDigests(release string) string {
	var b strings.Builder
	for i := range 3 {
		fmt.Fprintf(&b, "ghcr.io/siderolabs/module-%d:6.18-%s@sha256:%s\n", i, release, hexSHA256([]byte(fmt.Sprint(i, release))))
	}
	for _, kind := range []string{"lts", "production"} {
		fmt.Fprintf(&b, "ghcr.io/siderolabs/nvidia-container-toolkit-%s:580.1-v1.17.5@sha256:%s\n", kind, hexSHA256([]byte(kind)))
	}
	b.WriteString(agentLine(release) + "\n")
	return b.String()
}

func agentOf(release string) string { return "11.1.1@sha256:" + hexSHA256([]byte("agent"+release)) }

func agentLine(release string) string { return talosGuestAgent + agentOf(release) }

// fakeSidero is ghcr.io's registry API for Sidero's three repositories, with its token service.
type fakeSidero struct {
	mu    sync.Mutex
	blobs map[string][]byte
	tags  map[string]map[string]string
	// pins are the pins of the releases as served.
	pins map[string]templates.TalosPin
	// layerFiles are the files of each release's extensions layers, to write a layer again.
	layerFiles map[string][][]tarFile

	paths        []string
	challenged   map[string]int
	carried      map[string]int
	carriedRepo  map[string]int
	tokenFetches map[string]int
	tokens       map[string]bool
	sawGitHub    bool
	tokenAuth    bool
	script       map[string][]func(w http.ResponseWriter)
	tokenScript  map[string][]func(w http.ResponseWriter)
	expireFirst  map[string]int
	refuseAll    map[string]bool
	url          string
}

func newFakeSidero() *fakeSidero {
	return &fakeSidero{
		blobs: map[string][]byte{}, tags: map[string]map[string]string{}, pins: map[string]templates.TalosPin{}, layerFiles: map[string][][]tarFile{},
		challenged: map[string]int{}, carried: map[string]int{}, carriedRepo: map[string]int{}, tokenFetches: map[string]int{}, tokens: map[string]bool{},
		script: map[string][]func(w http.ResponseWriter){}, tokenScript: map[string][]func(w http.ResponseWriter){},
		expireFirst: map[string]int{}, refuseAll: map[string]bool{},
	}
}

func (r *fakeSidero) put(b []byte) string {
	d := "sha256:" + hexSHA256(b)
	r.blobs[d] = b
	return d
}

func (r *fakeSidero) tag(repo, release, digest string) {
	if r.tags[repo] == nil {
		r.tags[repo] = map[string]string{}
	}
	r.tags[repo][release] = digest
}

// add serves a release: its imager, installer base and extensions image, each as an index of one
// linux/amd64 manifest.
func (r *fakeSidero) add(release string, s releaseSpec) { //nolint:gocritic // a fixture's options, read once
	indexType, manifestType := ociIndex, ociManifest
	layerType := "application/vnd.oci.image.layer.v1.tar+gzip"
	if s.docker {
		indexType, manifestType = dockerIndex, dockerManifest
		layerType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	}
	index := func(manifest string, extra ...string) string {
		amd64 := fmt.Sprintf(`{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}`, manifest)
		entries := []string{amd64}
		for _, e := range extra {
			switch e {
			case "arm64":
				entries = append(entries, `{"digest":"sha256:`+pinA+`","platform":{"os":"linux","architecture":"arm64"}}`)
			case "attestation":
				entries = append(entries, `{"digest":"sha256:`+pinB+`","platform":{"os":"unknown","architecture":"unknown"}}`)
			case "no amd64":
				entries = entries[1:]
			case "two amd64":
				entries = append(entries, `{"digest":"sha256:`+pinA+`","platform":{"os":"linux","architecture":"amd64"}}`)
			}
		}
		return fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[%s]}`, indexType, strings.Join(entries, ","))
	}
	labelled := func(repo, label string, pad int) string {
		config := fmt.Sprintf(`{"architecture":"amd64","config":{"Labels":{%q:%q},"Env":["VERSION=%s"],"Entrypoint":[%q]}}`, talosVersionLabel, label, label, repo)
		if pad > 0 {
			config = config[:len(config)-1] + `,"pad":"` + strings.Repeat("x", pad-len(config)-9) + `"}`
		}
		cfg := r.put([]byte(config))
		m := r.put(fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":%q,"config":{"digest":%q},"layers":[]}`, manifestType, cfg))
		var extra []string
		if repo == "imager" {
			extra = s.platforms
		}
		return r.put([]byte(index(m, extra...)))
	}
	label := func(l string) string {
		if l == "" {
			return release
		}
		return l
	}
	pad := 0
	if s.bigConfig {
		pad = registryBodyLimit + 1
	}
	pin := templates.TalosPin{GuestAgent: agentOf(release)}
	imager := labelled("imager", label(s.imagerLabel), pad)
	installer := labelled("installer-base", label(s.installerLabel), 0)
	layers := s.layers
	if layers == nil {
		digests := s.digests
		if digests == "" {
			digests = goodDigests(release)
		}
		layers = [][]tarFile{{{"image-digests", digests}}}
	}
	r.layerFiles[release] = layers
	descriptors := make([]string, 0, len(layers))
	for _, files := range layers {
		d := r.put(gzipTar(files, time.Time{}))
		descriptors = append(descriptors, fmt.Sprintf(`{"mediaType":%q,"digest":%q}`, layerType, d))
	}
	ext := r.put(fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":%q,"config":{"digest":"sha256:%s"},"layers":[%s]}`, manifestType, pinA, strings.Join(descriptors, ",")))
	extIndex := r.put([]byte(index(ext)))
	pin.Imager, pin.InstallerBase, pin.Extensions = imager, installer, extIndex
	r.pins[release] = pin
	for repo, digest := range map[string]string{"imager": imager, "installer-base": installer, "extensions": extIndex} {
		if s.skip != repo {
			r.tag(repo, release, digest)
		}
	}
}

func (r *fakeSidero) challenge(w http.ResponseWriter, repo string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io",scope="repository:siderolabs/%s:pull"`, r.url, repo))
	w.WriteHeader(http.StatusUnauthorized)
}

func (r *fakeSidero) serveToken(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	scope := req.URL.Query().Get("scope")
	repo := strings.TrimSuffix(strings.TrimPrefix(scope, "repository:siderolabs/"), ":pull")
	r.tokenAuth = r.tokenAuth || req.Header.Get("Authorization") != ""
	r.tokenFetches[repo]++
	n := r.tokenFetches[repo]
	var scripted func(w http.ResponseWriter)
	if s := r.tokenScript[repo]; len(s) > 0 {
		scripted, r.tokenScript[repo] = s[0], s[1:]
	}
	token := fmt.Sprintf("tok-%s-%d", repo, n)
	r.tokens[token] = true
	r.mu.Unlock()
	if scripted != nil {
		scripted(w)
		return
	}
	answer, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(answer)
}

func (r *fakeSidero) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v2/siderolabs/"), "/")
	repo, ref := parts[0], parts[len(parts)-1]
	r.paths = append(r.paths, req.URL.Path)
	auth := req.Header.Get("Authorization")
	if auth == "" {
		r.challenged[repo]++
		r.mu.Unlock()
		r.challenge(w, repo)
		return
	}
	r.sawGitHub = r.sawGitHub || auth == "Bearer t"
	r.carried[req.URL.Path]++
	r.carriedRepo[repo]++
	token := strings.TrimPrefix(auth, "Bearer ")
	expired := token == "tok-"+repo+"-1" && r.expireFirst[repo] > 0 && r.carriedRepo[repo] > r.expireFirst[repo]
	if r.refuseAll[repo] || !r.tokens[token] || expired {
		r.mu.Unlock()
		r.challenge(w, repo)
		return
	}
	var scripted func(w http.ResponseWriter)
	if s := r.script[req.URL.Path]; len(s) > 0 {
		scripted, r.script[req.URL.Path] = s[0], s[1:]
	}
	digest := ref
	if d, ok := r.tags[repo][ref]; ok {
		digest = d
	}
	body, ok := r.blobs[digest]
	r.mu.Unlock()
	switch {
	case scripted != nil:
		scripted(w)
	case !ok:
		w.WriteHeader(http.StatusNotFound)
	default:
		_, _ = w.Write(body)
	}
}

// regPath is the address of a manifest or blob.
func regPath(repo, kind, ref string) string { return "/v2/siderolabs/" + repo + "/" + kind + "/" + ref }

// start serves the registry beside a GitHub that lists releases, and returns a pinner for both.
func (r *fakeSidero) start(t *testing.T, github map[string][]githubRelease) *ReleasePinner {
	t.Helper()
	files := &fakeReleases{files: map[string]string{}}
	api := &githubAPI{files: files, token: "t", releases: github}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/token":
			r.serveToken(w, req)
		case strings.HasPrefix(req.URL.Path, "/v2/"):
			r.serve(w, req)
		default:
			api.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return &ReleasePinner{
		HTTP: srv.Client(), API: srv.URL, Registry: srv.URL, Token: "t",
		Checksums: &ReleaseChecksums{HTTP: srv.Client(), GitHub: srv.URL, Raw: srv.URL}, retryWait: time.Millisecond,
	}
}

func talosListing(tags ...string) map[string][]githubRelease {
	list := make([]githubRelease, len(tags))
	for i, t := range tags {
		list[i] = tag(t)
	}
	return map[string][]githubRelease{"siderolabs/talos": list}
}

// only v1.14.6 listed and served, as every refusal case starts.
func serveOne(t *testing.T, s releaseSpec) (*fakeSidero, *ReleasePinner) { //nolint:gocritic // a fixture's options, read once
	t.Helper()
	r := newFakeSidero()
	r.add("v1.14.6", s)
	p := r.start(t, talosListing("v1.14.6"))
	return r, p
}

func (r *fakeSidero) requestsFor(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.paths {
		if strings.Contains(p, substr) {
			n++
		}
	}
	return n
}

func TestPinAddsTheNewTalosReleasesOnce(t *testing.T) {
	t.Parallel()
	r := newFakeSidero()
	for _, v := range []string{"v1.14.6", "v1.15.0", "v1.14.5", "v1.14.7", "v1.10.3"} {
		r.add(v, releaseSpec{})
	}
	r.add("v1.13.9", releaseSpec{docker: true})
	github := map[string][]githubRelease{"siderolabs/talos": {
		{Tag: "v1.15.0", Draft: true}, {Tag: "v1.14.7", Prerelease: true}, tag("v1.14.6"), tag("v1.14.5"), tag("v1.13.9"), tag("v1.10.3"), tag("v1.9.9"),
	}}
	p := r.start(t, github)
	kept := ReleasePin{Talos: r.pins["v1.14.5"]}
	keptOld := ReleasePin{Talos: r.pins["v1.10.3"]}
	pins := ReleasePins{"v1.14.5": kept, "v1.10.3": keptOld}

	added, err := p.Pin(context.Background(), pins)
	if err != nil || !slices.Equal(added, []string{"v1.14.6", "v1.13.9"}) {
		t.Fatalf("Pin = %v, %v", added, err)
	}
	for _, v := range added {
		if pins[v] != (ReleasePin{Talos: r.pins[v]}) {
			t.Errorf("%s pinned as %+v, served %+v", v, pins[v], r.pins[v])
		}
	}
	if pins["v1.14.5"] != kept || pins["v1.10.3"] != keptOld {
		t.Error("a kept pin changed")
	}
	if _, ok := pins["v1.14.7"]; ok {
		t.Error("a pre-release was pinned")
	}
	if _, ok := pins["v1.9.9"]; ok {
		t.Error("a release below the window was pinned")
	}
	for _, repo := range []string{"imager", "installer-base", "extensions"} {
		if r.tokenFetches[repo] != 1 || r.challenged[repo] != 1 {
			t.Errorf("%s: %d token fetches and %d challenges, want one of each for the run", repo, r.tokenFetches[repo], r.challenged[repo])
		}
	}
	if r.sawGitHub || r.tokenAuth {
		t.Error("GitHub's token or any credential reached the registry")
	}

	again, err := p.Pin(context.Background(), pins)
	if err != nil || len(again) != 0 {
		t.Errorf("a second run added %v, %v", again, err)
	}

	github["siderolabs/talos"] = []githubRelease{tag("v1.15.0"), tag("v1.14.6"), tag("v1.10.4"), tag("v1.10.3")}
	late, err := p.Pin(context.Background(), pins)
	if err != nil || !slices.Equal(late, []string{"v1.15.0"}) {
		t.Fatalf("Pin after v1.15.0 = %v, %v", late, err)
	}
	if _, ok := pins["v1.10.4"]; ok || r.requestsFor("v1.10.4") != 0 {
		t.Error("v1.10.4, below the window, was read")
	}
	if pins["v1.10.3"] != keptOld {
		t.Error("a pin below the window was dropped or changed")
	}
}

func TestPinReturnsWhatItAddedBeforeAnError(t *testing.T) {
	t.Parallel()
	r := newFakeSidero()
	r.add("v1.14.6", releaseSpec{})
	r.add("v1.13.9", releaseSpec{skip: "installer-base"})
	github := talosListing("v1.14.6", "v1.13.9")
	github["rancher/rke2"] = []githubRelease{tag("v1.37.1+rke2r1")}
	p := r.start(t, github)
	// start's server has no release files, so a second one serves the RKE2 release's.
	files := &fakeReleases{files: map[string]string{}}
	maps.Copy(files.files, releaseFiles(rke2Release, "v1.37.1+rke2r1", rke2Listing))
	fileSrv := httptest.NewServer(files)
	t.Cleanup(fileSrv.Close)
	p.Checksums = &ReleaseChecksums{HTTP: fileSrv.Client(), GitHub: fileSrv.URL, Raw: fileSrv.URL}

	pins := ReleasePins{}
	added, err := p.Pin(context.Background(), pins)
	if !slices.Equal(added, []string{"v1.37.1+rke2r1", "v1.14.6"}) {
		t.Errorf("added %v", added)
	}
	if err == nil || !strings.Contains(err.Error(), "v1.13.9") || !strings.Contains(err.Error(), "installer base") {
		t.Errorf("error %v, want one naming v1.13.9 and installer base", err)
	}
	if _, ok := pins["v1.37.1+rke2r1"]; !ok {
		t.Error("the RKE2 pin was dropped")
	}
	if _, ok := pins["v1.14.6"]; !ok {
		t.Error("the Talos pin was dropped")
	}
}

func TestPinTalosReadsImageDigestsByItsExactName(t *testing.T) {
	t.Parallel()
	good := goodDigests("v1.14.6")
	decoy := "ghcr.io/siderolabs/module-9:6.18-v1.14.5@sha256:" + pinA + "\n" + talosGuestAgent + "9.9.9@sha256:" + pinB + "\n"
	for _, tc := range []struct {
		name   string
		layers [][]tarFile
		want   string
	}{
		{"a member in a directory is passed over", [][]tarFile{{{"docs/image-digests", decoy}}, {{"image-digests", good}}}, agentOf("v1.14.6")},
		{"a leading ./ names the same file", [][]tarFile{{{"./image-digests", good}}}, agentOf("v1.14.6")},
		{"a member in a directory is no match", [][]tarFile{{{"docs/image-digests", good}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, p := serveOne(t, releaseSpec{layers: tc.layers})
			pins := ReleasePins{}
			added, err := p.Pin(context.Background(), pins)
			if tc.want == "" {
				if len(added) != 0 || err == nil || !strings.Contains(err.Error(), "v1.14.6") || !strings.Contains(err.Error(), "extensions image") || !strings.Contains(err.Error(), "no image-digests") {
					t.Errorf("Pin = %v, %v", added, err)
				}
				return
			}
			if err != nil || !slices.Equal(added, []string{"v1.14.6"}) || pins["v1.14.6"].Talos.GuestAgent != tc.want {
				t.Errorf("Pin = %v, %v, pins %+v", added, err, pins)
			}
		})
	}
}

func TestPinTalosRefusesWhatDoesNotNameItsRelease(t *testing.T) {
	t.Parallel()
	toolkit := func(n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "ghcr.io/siderolabs/nvidia-%d:580.1-v1.17.5@sha256:%s\n", i, hexSHA256([]byte{byte(i)}))
		}
		return b.String()
	}
	own := func(n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "ghcr.io/siderolabs/module-%d:6.18-v1.14.6@sha256:%s\n", i, hexSHA256([]byte{byte(i), 1}))
		}
		return b.String()
	}
	agent := agentLine("v1.14.6") + "\n"
	for _, tc := range []struct {
		name string
		spec releaseSpec
		want []string
	}{
		{"an imager of another release", releaseSpec{imagerLabel: "v1.14.5"}, []string{"imager"}},
		{"an installer base of another release", releaseSpec{installerLabel: "v1.14.5"}, []string{"installer base"}},
		{"no line of its own", releaseSpec{digests: toolkit(2) + agent}, []string{"extensions image", "-v1.14.6 on 0 lines", "-v1.17.5 on 2"}},
		{"a tie with another suffix", releaseSpec{digests: own(2) + toolkit(2) + agent}, []string{"extensions image", "-v1.14.6 on 2 lines", "-v1.17.5 on 2"}},
		{"no agent line", releaseSpec{digests: own(3)}, []string{"extensions image", "0 lines of the QEMU guest agent"}},
		{"two agent lines", releaseSpec{digests: own(3) + agent + talosGuestAgent + "9.9.9@sha256:" + pinB + "\n"}, []string{"extensions image", "2 lines of the QEMU guest agent"}},
		{"a short agent digest", releaseSpec{digests: own(3) + talosGuestAgent + "11.1.1@sha256:" + pinA[:63] + "\n"}, []string{"extensions image", "11.1.1@sha256:" + pinA[:63]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, p := serveOne(t, tc.spec)
			pins := ReleasePins{}
			added, err := p.Pin(context.Background(), pins)
			if len(added) != 0 || len(pins) != 0 || err == nil || !strings.Contains(err.Error(), "v1.14.6") {
				t.Fatalf("Pin = %v, %v", added, err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
	t.Run("a 404 for the installer base", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{skip: "installer-base"})
		added, err := p.Pin(context.Background(), ReleasePins{})
		if len(added) != 0 || err == nil || !strings.Contains(err.Error(), "installer base") {
			t.Fatalf("Pin = %v, %v", added, err)
		}
		if n := r.carried[regPath("installer-base", "manifests", "v1.14.6")]; n != 1 {
			t.Errorf("%d requests for the missing index, want 1", n)
		}
	})
}

func TestTalosImagesRefusesABodyOffItsDigest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		pick   func(r *fakeSidero, pin templates.TalosPin) string
		change func(r *fakeSidero, body []byte) []byte
		want   string
	}{
		{"the imager's index", func(_ *fakeSidero, pin templates.TalosPin) string { return pin.Imager }, func(_ *fakeSidero, b []byte) []byte { return append(b, ' ') }, "imager"},
		{"its amd64 manifest", func(r *fakeSidero, pin templates.TalosPin) string { return r.firstManifest(pin.Imager) }, func(_ *fakeSidero, b []byte) []byte { return append(b, ' ') }, "imager"},
		{"its config blob", func(r *fakeSidero, pin templates.TalosPin) string { return r.configOf(r.firstManifest(pin.Imager)) }, func(_ *fakeSidero, b []byte) []byte { return append(b, ' ') }, "imager"},
		{"the layer holding image-digests", func(r *fakeSidero, pin templates.TalosPin) string { return r.layerOf(pin.Extensions) }, func(r *fakeSidero, _ []byte) []byte {
			return gzipTar(r.layerFiles["v1.14.6"][0], time.Unix(1_000_000, 0))
		}, "extensions image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, p := serveOne(t, releaseSpec{})
			pin := r.pins["v1.14.6"]
			digest := tc.pick(r, pin)
			r.blobs[digest] = tc.change(r, r.blobs[digest])
			err := p.CheckTalos(context.Background(), ReleasePins{"v1.14.6": {Talos: pin}})
			if err == nil || !strings.Contains(err.Error(), "v1.14.6") || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), digest) {
				t.Errorf("CheckTalos = %v, want one naming v1.14.6, %s and %s", err, tc.want, digest)
			}
		})
	}
}

// firstManifest is the digest of the first manifest of an index.
func (r *fakeSidero) firstManifest(index string) string {
	var doc manifestDocument
	_ = json.Unmarshal(r.blobs[index], &doc)
	return doc.Manifests[0].Digest
}

func (r *fakeSidero) configOf(manifest string) string {
	var doc manifestDocument
	_ = json.Unmarshal(r.blobs[manifest], &doc)
	return doc.Config.Digest
}

func (r *fakeSidero) layerOf(index string) string {
	var doc manifestDocument
	_ = json.Unmarshal(r.blobs[r.firstManifest(index)], &doc)
	return doc.Layers[0].Digest
}

func TestTalosImagesRetriesWhatMayPass(t *testing.T) {
	t.Parallel()
	index := regPath("imager", "manifests", "v1.14.6")
	t.Run("a 429 and a 503 pass", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{})
		r.script[index] = []func(http.ResponseWriter){answerStatus(http.StatusTooManyRequests), answerStatus(http.StatusServiceUnavailable)}
		added, err := p.Pin(context.Background(), ReleasePins{})
		if err != nil || !slices.Equal(added, []string{"v1.14.6"}) || r.carried[index] != 3 {
			t.Errorf("Pin = %v, %v after %d requests, want one release after 3", added, err, r.carried[index])
		}
	})
	t.Run("four 503s end it", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{})
		fail := answerStatus(http.StatusServiceUnavailable)
		r.script[index] = []func(http.ResponseWriter){fail, fail, fail, fail, nil}
		_, err := p.Pin(context.Background(), ReleasePins{})
		if err == nil || !strings.Contains(err.Error(), "imager") || !strings.Contains(err.Error(), "503") || r.carried[index] != 4 {
			t.Errorf("Pin = %v after %d requests, want an error naming imager and 503 after 4", err, r.carried[index])
		}
	})
	t.Run("a token fetch is tried again", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{})
		r.tokenScript["imager"] = []func(http.ResponseWriter){answerStatus(http.StatusServiceUnavailable)}
		added, err := p.Pin(context.Background(), ReleasePins{})
		if err != nil || len(added) != 1 || r.tokenFetches["imager"] != 2 {
			t.Errorf("Pin = %v, %v after %d token fetches, want 2", added, err, r.tokenFetches["imager"])
		}
	})
	t.Run("a body cut short is tried again", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{})
		r.script[index] = []func(http.ResponseWriter){cutOff}
		added, err := p.Pin(context.Background(), ReleasePins{})
		if err != nil || len(added) != 1 || r.carried[index] != 2 {
			t.Errorf("Pin = %v, %v after %d requests, want 2", added, err, r.carried[index])
		}
	})
	t.Run("a 404 is not", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{})
		r.script[index] = []func(http.ResponseWriter){answerStatus(http.StatusNotFound)}
		_, err := p.Pin(context.Background(), ReleasePins{})
		if err == nil || r.carried[index] != 1 {
			t.Errorf("Pin = %v after %d requests, want an error after 1", err, r.carried[index])
		}
	})
}

func TestTalosImagesRenewsATokenOnce(t *testing.T) {
	t.Parallel()
	t.Run("an expired token is replaced", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{})
		r.expireFirst["imager"] = 2
		added, err := p.Pin(context.Background(), ReleasePins{})
		if err != nil || len(added) != 1 || r.tokenFetches["imager"] != 2 {
			t.Errorf("Pin = %v, %v after %d imager token fetches, want 2", added, err, r.tokenFetches["imager"])
		}
	})
	t.Run("a token always refused ends it", func(t *testing.T) {
		t.Parallel()
		r, p := serveOne(t, releaseSpec{})
		r.refuseAll["imager"] = true
		added, err := p.Pin(context.Background(), ReleasePins{})
		index := regPath("imager", "manifests", "v1.14.6")
		if len(added) != 0 || err == nil || !strings.Contains(err.Error(), "imager") || !strings.Contains(err.Error(), "401 with a fresh token") {
			t.Errorf("Pin = %v, %v", added, err)
		}
		if r.tokenFetches["imager"] != 2 || r.carried[index] != 2 {
			t.Errorf("%d token fetches and %d token-carrying requests, want 2 and 2", r.tokenFetches["imager"], r.carried[index])
		}
	})
}

func TestTalosImagesRefusesABodyOverItsCap(t *testing.T) {
	t.Parallel()
	t.Run("a config blob", func(t *testing.T) {
		t.Parallel()
		_, p := serveOne(t, releaseSpec{bigConfig: true})
		added, err := p.Pin(context.Background(), ReleasePins{})
		if len(added) != 0 || err == nil || !strings.Contains(err.Error(), "imager") || !strings.Contains(err.Error(), "larger than 1 MiB") {
			t.Errorf("Pin = %v, %v", added, err)
		}
	})
	t.Run("an image-digests after gunzip", func(t *testing.T) {
		t.Parallel()
		big := goodDigests("v1.14.6") + strings.Repeat("#", registryBodyLimit)
		_, p := serveOne(t, releaseSpec{layers: [][]tarFile{{{"image-digests", big}}}})
		added, err := p.Pin(context.Background(), ReleasePins{})
		if len(added) != 0 || err == nil || !strings.Contains(err.Error(), "extensions image") || !strings.Contains(err.Error(), "larger than 1 MiB") {
			t.Errorf("Pin = %v, %v", added, err)
		}
	})
}

func TestTalosImagesTakesTheOneAmd64Manifest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		platforms []string
		want      string
	}{
		{"others beside it", []string{"arm64", "attestation"}, ""},
		{"none", []string{"no amd64"}, "0 linux/amd64 manifests"},
		{"two", []string{"two amd64"}, "2 linux/amd64 manifests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, p := serveOne(t, releaseSpec{platforms: tc.platforms})
			added, err := p.Pin(context.Background(), ReleasePins{})
			if tc.want == "" {
				if err != nil || len(added) != 1 {
					t.Errorf("Pin = %v, %v", added, err)
				}
				return
			}
			if len(added) != 0 || err == nil || !strings.Contains(err.Error(), "imager") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Pin = %v, %v, want an error naming imager and %q", added, err, tc.want)
			}
		})
	}
}

func TestCheckTalosReadsEachPinByDigest(t *testing.T) {
	t.Parallel()
	r := newFakeSidero()
	for _, v := range []string{"v1.14.6", "v1.14.5", "v1.13.9", "v1.10.3"} {
		r.add(v, releaseSpec{})
	}
	r.add("v1.12.1", releaseSpec{docker: true})
	r.add("v1.17.5", releaseSpec{})
	p := r.start(t, nil)
	pins := ReleasePins{"v1.37.1+rke2r1": {File: pinA, Script: pinB}}
	for _, v := range []string{"v1.14.6", "v1.14.5", "v1.13.9", "v1.12.1", "v1.10.3"} {
		pins[v] = ReleasePin{Talos: r.pins[v]}
	}
	if err := p.CheckTalos(context.Background(), pins); err != nil {
		t.Fatal(err)
	}
	for _, served := range r.paths {
		if strings.Contains(served, "/manifests/v1.") {
			t.Errorf("read the tag in %s", served)
		}
		if strings.Contains(served, "+rke2") {
			t.Errorf("an RKE2 pin was read: %s", served)
		}
	}
	// A tag that moves changes nothing, as the pin names a digest.
	r.tag("extensions", "v1.14.6", r.pins["v1.14.5"].Extensions)
	if err := p.CheckTalos(context.Background(), pins); err != nil {
		t.Errorf("a moved tag failed the check: %v", err)
	}

	// One pinner holds one run's tokens, so these run one after another.
	t.Run("another release's imager", func(t *testing.T) {
		bad := maps.Clone(pins)
		pin := r.pins["v1.14.6"]
		pin.Imager = r.pins["v1.14.5"].Imager
		bad["v1.14.6"] = ReleasePin{Talos: pin}
		if err := p.CheckTalos(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "v1.14.6") || !strings.Contains(err.Error(), "imager") {
			t.Errorf("CheckTalos = %v", err)
		}
	})
	t.Run("the toolkit's suffix", func(t *testing.T) {
		pin := templates.TalosPin{Imager: r.pins["v1.17.5"].Imager, InstallerBase: r.pins["v1.17.5"].InstallerBase, Extensions: r.pins["v1.14.6"].Extensions, GuestAgent: r.pins["v1.14.6"].GuestAgent}
		err := p.CheckTalos(context.Background(), ReleasePins{"v1.17.5": {Talos: pin}})
		if err == nil || !strings.Contains(err.Error(), "v1.17.5") || !strings.Contains(err.Error(), "extensions image") || !strings.Contains(err.Error(), "-v1.17.5 on 2") || !strings.Contains(err.Error(), "-v1.14.6 on 3") {
			t.Errorf("CheckTalos = %v", err)
		}
	})
	t.Run("another agent", func(t *testing.T) {
		pin := r.pins["v1.14.5"]
		pin.GuestAgent = "9.9.9@sha256:" + pinB
		err := p.CheckTalos(context.Background(), ReleasePins{"v1.14.5": {Talos: pin}})
		if err == nil || !strings.Contains(err.Error(), pin.GuestAgent) || !strings.Contains(err.Error(), agentOf("v1.14.5")) {
			t.Errorf("CheckTalos = %v", err)
		}
	})
	t.Run("a digest no longer served", func(t *testing.T) {
		t.Parallel()
		gone, gp := serveOne(t, releaseSpec{})
		pin := gone.pins["v1.14.6"]
		delete(gone.blobs, pin.InstallerBase)
		err := gp.CheckTalos(context.Background(), ReleasePins{"v1.14.6": {Talos: pin}})
		if err == nil || !strings.Contains(err.Error(), "delete the line and run make release-pins") || !strings.Contains(err.Error(), "installer base") || !strings.Contains(err.Error(), pin.InstallerBase) {
			t.Errorf("CheckTalos = %v", err)
		}
	})
}
