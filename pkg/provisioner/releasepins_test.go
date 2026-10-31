package provisioner

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

var (
	pinA = strings.Repeat("a", 64)
	pinB = strings.Repeat("b", 64)
)

func TestParseReleasePinsRefusesAMalformedFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, file, want string }{
		{"a version of neither distribution", "v1.37.1 " + pinA + " " + pinB, "line 1"},
		{"a version that would reach a node's shell", "v1.37.1+k3s1;reboot " + pinA + " " + pinB, "line 1"},
		{"a short checksum", "# header\nv1.37.1+k3s1 " + pinA[:63] + " " + pinB, "line 2"},
		{"a missing script checksum", "v1.37.1+k3s1 " + pinA, "line 1"},
		{"an uppercase checksum", "v1.37.1+k3s1 " + strings.ToUpper(pinA) + " " + pinB, "line 1"},
		{"a release twice", "v1.37.1+k3s1 " + pinA + " " + pinB + "\nv1.37.1+k3s1 " + pinB + " " + pinA, "line 2: v1.37.1+k3s1 twice"},
	} {
		if _, err := ParseReleasePins(tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", tc.name, err, tc.want)
		}
	}
	pins, err := ParseReleasePins("# comment\n\nv1.37.1+k3s1 " + pinA + " " + pinB + "\n  v1.36.5+rke2r2 " + pinB + " " + pinA + "  \n")
	if err != nil || len(pins) != 2 || pins["v1.37.1+k3s1"] != (ReleasePin{pinA, pinB}) || pins["v1.36.5+rke2r2"] != (ReleasePin{pinB, pinA}) {
		t.Errorf("pins %v, %v", pins, err)
	}
}

// The newest release of each distribution, which Renovate raises in the header, has to be pinned
// before the change can land; and the file is kept as make release-pins writes it.
func TestTheEmbeddedPinsHoldTheNewestReleases(t *testing.T) {
	t.Parallel()
	pins := DefaultReleasePins()
	newest := regexp.MustCompile(`(?m)^# (rke2|k3s) = (\S+)$`).FindAllStringSubmatch(releasePinsFile, -1)
	if len(newest) != 2 {
		t.Fatalf("the header names %d newest releases, want rke2's and k3s's", len(newest))
	}
	for _, n := range newest {
		release := distributionReleaseOf(n[2])
		if release == nil || string(release.distribution) != n[1] {
			t.Errorf("the header's %s = %s is no %s release", n[1], n[2], n[1])
			continue
		}
		if _, ok := pins[n[2]]; !ok {
			t.Errorf("%s is not pinned: run make release-pins", n[2])
		}
		v, _ := parseKubeVersion(n[2])
		for m := v.Minor - supportedMinors; m <= v.Minor; m++ {
			if _, ok := pins.newest(release, m); !ok {
				t.Errorf("no %s release of 1.%d is pinned", n[1], m)
			}
		}
	}
	if got := pins.Format(PinsHeader(releasePinsFile)); got != releasePinsFile {
		t.Error("releasepins.txt is not as make release-pins writes it: run make release-pins")
	}
}

func TestReleasePinsFormatSortsByDistributionAndVersion(t *testing.T) {
	t.Parallel()
	pins := ReleasePins{
		"v1.37.1+k3s1":    {pinA, pinB},
		"v1.36.10+rke2r1": {pinA, pinB},
		"v1.36.9+rke2r2":  {pinA, pinB},
		"v1.36.9+rke2r1":  {pinA, pinB},
		"v1.36.5+k3s1":    {pinB, pinA},
	}
	got := pins.Format("# pins\n")
	want := "# pins\n" +
		"v1.36.9+rke2r1 " + pinA + " " + pinB + "\n" +
		"v1.36.9+rke2r2 " + pinA + " " + pinB + "\n" +
		"v1.36.10+rke2r1 " + pinA + " " + pinB + "\n" +
		"v1.36.5+k3s1 " + pinB + " " + pinA + "\n" +
		"v1.37.1+k3s1 " + pinA + " " + pinB + "\n"
	if got != want {
		t.Errorf("formatted\n%s\nwant\n%s", got, want)
	}
	again, err := ParseReleasePins(got)
	if err != nil || !maps.Equal(again, pins) {
		t.Errorf("read back %v, %v", again, err)
	}
	if h := PinsHeader("# a\n# b\nv1.37.1+k3s1 x y\n# c\n"); h != "# a\n# b\n" {
		t.Errorf("header %q", h)
	}
}

func TestReleasePinsOfferTheNewestOfEachMinor(t *testing.T) {
	t.Parallel()
	pins := ReleasePins{"v1.36.5+rke2r1": {}, "v1.36.5+rke2r2": {}, "v1.37.1+rke2r1": {}, "v1.35.9+rke2r1": {}, "v1.38.0+k3s1": {}}
	if got := pins.offered(rke2Release); !slices.Equal(got, []string{"v1.37.1+rke2r1", "v1.36.5+rke2r2", "v1.35.9+rke2r1"}) {
		t.Errorf("offered %v", got)
	}
	if _, ok := pins.newest(rke2Release, 38); ok {
		t.Error("a k3s release counted as RKE2's")
	}
}

type githubRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// githubAPI serves the releases API's pages as GitHub does, after the releases' files.
type githubAPI struct {
	files    *fakeReleases
	releases map[string][]githubRelease
	token    string
}

func (g *githubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	repo, ok := strings.CutPrefix(r.URL.Path, "/repos/")
	if !ok {
		g.files.ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+g.token {
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}
	list := g.releases[strings.TrimSuffix(repo, "/releases")]
	page := []githubRelease{}
	if r.URL.Query().Get("page") == "1" && r.URL.Query().Get("per_page") == "100" {
		page = list
	}
	if err := json.NewEncoder(w).Encode(page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func tag(name string) githubRelease { return githubRelease{Tag: name} }

func TestReleasePinnerPinsTheNewMinorsReleasesOnce(t *testing.T) {
	t.Parallel()
	f := &fakeReleases{files: map[string]string{}}
	for _, r := range []struct {
		release *distributionRelease
		version string
		listed  string
	}{
		{rke2Release, "v1.37.1+rke2r1", rke2Listing},
		{rke2Release, "v1.33.0+rke2r1", rke2Listing},
		{rke2Release, "v1.32.9+rke2r1", rke2Listing},
		{rke2Release, "v1.36.0+rke2r1", listing("rke2.linux-amd64.tar.gz", "rke2-images-core.linux-amd64.tar.zst")},
		{rke2Release, "v1.38.0-rc1+rke2r1", rke2Listing},
		{k3sRelease, "v1.37.1+k3s1", k3sListing},
		{k3sRelease, "v1.37.2+k3s1", k3sListing},
	} {
		maps.Copy(f.files, releaseFiles(r.release, r.version, r.listed))
	}
	api := &githubAPI{files: f, token: "t", releases: map[string][]githubRelease{
		"rancher/rke2": {
			{Tag: "v1.38.0+rke2r1", Draft: true},
			{Tag: "v1.38.0-rc1+rke2r1", Prerelease: true},
			tag("v1.37.1+rke2r1"),
			// Published, but its files never came.
			tag("v1.37.0+rke2r1"),
			// Its checksums miss the CNIs' images.
			tag("v1.36.0+rke2r1"),
			tag("v1.33.0+rke2r1"),
			// Older than any cluster may run.
			tag("v1.32.9+rke2r1"),
		},
		"k3s-io/k3s": {tag("v1.37.2+k3s1"), tag("v1.37.1+k3s1")},
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	p := &ReleasePinner{HTTP: srv.Client(), API: srv.URL, Token: "t", Checksums: &ReleaseChecksums{HTTP: srv.Client(), GitHub: srv.URL, Raw: srv.URL}}
	kept := ReleasePin{pinA, pinB}
	pins := ReleasePins{"v1.37.1+k3s1": kept}
	added, err := p.Pin(context.Background(), pins)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(added, []string{"v1.37.1+rke2r1", "v1.33.0+rke2r1", "v1.37.2+k3s1"}) {
		t.Errorf("added %v", added)
	}
	if pins["v1.37.1+k3s1"] != kept {
		t.Error("a pin changed")
	}
	if pins["v1.37.1+rke2r1"] != f.pin(rke2Release, "v1.37.1+rke2r1") || pins["v1.37.2+k3s1"] != f.pin(k3sRelease, "v1.37.2+k3s1") {
		t.Errorf("pins %v", pins)
	}
	if again, err := p.Pin(context.Background(), pins); err != nil || len(again) != 0 {
		t.Errorf("a second run added %v, %v", again, err)
	}
	p.Token = "wrong"
	if _, err := p.Pin(context.Background(), ReleasePins{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused API read: %v", err)
	}
}

func TestChannelsOfferOnlyPinnedReleases(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"stable","latest":"v1.36.6+rke2r1"},{"id":"v1.35","latest":"v1.35.9+rke2r1"},
			{"id":"v1.36","latest":"v1.36.6+rke2r1"},{"id":"v1.37","latest":"v1.37.2+rke2r1"}]}`))
	}))
	defer srv.Close()
	s := &ChannelServer{HTTP: srv.Client(), URLs: map[string]string{"rke2": srv.URL}, Pins: ReleasePins{
		"v1.36.5+rke2r1": {}, "v1.36.5+rke2r2": {}, "v1.37.1+rke2r1": {}, "v1.35.9+k3s1": {},
	}}
	ch, err := s.Channels(context.Background(), "rke2", templates.DefaultNodeOS)
	if err != nil {
		t.Fatal(err)
	}
	if ch.StableMinor != 36 || len(ch.Latest) != 2 || ch.Latest[36].Raw != "v1.36.5+rke2r2" || ch.Latest[37].Raw != "v1.37.1+rke2r1" {
		t.Errorf("channels %+v", ch)
	}
}

func TestClusterValidatorHoldsRKE2AndK3sToPinnedReleases(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	r, _ := newReleaseChecksums(t)
	h.Releases = r
	v := &ClusterValidator{Hooks: h}
	c := cluster()
	c.Spec.Topology.ControlPlane.Replicas = new(int32(1))
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	c.Spec.Topology.Version = "v1.37.2+rke2r1"
	_, err := v.ValidateCreate(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "pins no rke2 release v1.37.2+rke2r1") || !strings.Contains(err.Error(), "v1.37.1+rke2r1, v1.36.5+rke2r1") {
		t.Fatalf("an unpinned release: %v", err)
	}
	c.Spec.Topology.Version = "v1.37.1+rke2r1"
	if _, err := v.ValidateCreate(context.Background(), c); err != nil {
		t.Fatalf("a pinned release: %v", err)
	}
	// A cluster on a release a later Kubehenge no longer pins may still change anything but its version.
	old := c.DeepCopy()
	old.Spec.Topology.Version = "v1.36.4+rke2r1"
	stays := old.DeepCopy()
	stays.Labels = map[string]string{"team": "a"}
	if _, err := v.ValidateUpdate(context.Background(), old, stays); err != nil {
		t.Errorf("a change beside the version: %v", err)
	}
	moves := old.DeepCopy()
	moves.Spec.Topology.Version = "v1.36.6+rke2r1"
	if _, err := v.ValidateUpdate(context.Background(), old, moves); err == nil || !strings.Contains(err.Error(), "pins no rke2 release") {
		t.Errorf("an upgrade to an unpinned release: %v", err)
	}
}
