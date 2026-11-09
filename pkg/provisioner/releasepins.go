package provisioner

import (
	"bufio"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	relver "github.com/kubehenge/kubehenge/pkg/release"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

//go:embed releasepins.txt
var releasePinsFile string

// ReleasePin is what a node of an RKE2 or k3s release checks before it installs anything: the
// SHA-256 of the release's checksum file, which lists every file of the release with its own, and
// of its install script. A Talos release's pin is Talos, the digests of its images, and the other
// two are empty.
type ReleasePin struct {
	File, Script string
	Talos        templates.TalosPin
}

// ReleasePins are the pins of every RKE2, k3s and Talos release a cluster may run, by version.
// Neither RKE2 nor k3s signs its releases or images, and a tag or an asset can be replaced. A
// release Kubehenge pins is installed only as it was when it was pinned, and one it does not pin
// not at all.
type ReleasePins map[string]ReleasePin

// talosGuestAgent is the image the guest agent of every Talos pin is a tag and digest of.
const talosGuestAgent = "ghcr.io/siderolabs/qemu-guest-agent:"

// Talos gives the Talos pins by release.
func (p ReleasePins) Talos() map[string]templates.TalosPin {
	out := map[string]templates.TalosPin{}
	for version, pin := range p {
		if talosRelease.holds(version) && pin.Talos != (templates.TalosPin{}) {
			out[version] = pin.Talos
		}
	}
	return out
}

// DefaultReleasePins are the pins this build of Kubehenge carries, releasepins.txt.
func DefaultReleasePins() ReleasePins {
	pins, err := ParseReleasePins(releasePinsFile)
	if err != nil {
		// The file is part of the build, and a unit test reads it.
		panic(err)
	}
	return pins
}

// ParseReleasePins reads a pins file: a line "<version> <checksum file's SHA-256> <install
// script's SHA-256>" for each RKE2 or k3s release, "<version> <imager> <installer base> <extensions
// image> <guest agent>" for each Talos release, and comments starting with #.
func ParseReleasePins(file string) (ReleasePins, error) {
	pins := ReleasePins{}
	lines := bufio.NewScanner(strings.NewReader(file))
	for n := 1; lines.Scan(); n++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A pin an administrator added ends in a comment saying who added it.
		line, _, _ = strings.Cut(line, " #")
		fields := strings.Fields(line)
		pin, ok := parsePin(fields)
		if !ok {
			return nil, fmt.Errorf("release pins, line %d: not an RKE2 or k3s version and two SHA-256s, nor a Talos release and its four images: %q", n, line)
		}
		if _, dup := pins[fields[0]]; dup {
			return nil, fmt.Errorf("release pins, line %d: %s twice", n, fields[0])
		}
		pins[fields[0]] = pin
	}
	return pins, nil
}

// The fields of a pin line: a version and two SHA-256s, or a version and four images.
const (
	checksumPinFields = 3
	talosPinFields    = 5
)

// parsePin reads the fields of one pin line.
func parsePin(fields []string) (ReleasePin, bool) {
	switch len(fields) {
	case checksumPinFields:
		if distributionReleaseOf(fields[0]) == nil || !sha256Hex.MatchString(fields[1]) || !sha256Hex.MatchString(fields[2]) {
			return ReleasePin{}, false
		}
		return ReleasePin{File: fields[1], Script: fields[2]}, true
	case talosPinFields:
		if !talosRelease.holds(fields[0]) {
			return ReleasePin{}, false
		}
		for _, d := range fields[1:4] {
			if hex, ok := strings.CutPrefix(d, "sha256:"); !ok || !sha256Hex.MatchString(hex) {
				return ReleasePin{}, false
			}
		}
		agent, err := ParseImage(talosGuestAgent + fields[4])
		if err != nil || agent.Registry != "ghcr.io" || agent.Repository != "siderolabs/qemu-guest-agent" {
			return ReleasePin{}, false
		}
		return ReleasePin{Talos: templates.TalosPin{Imager: fields[1], InstallerBase: fields[2], Extensions: fields[3], GuestAgent: fields[4]}}, true
	}
	return ReleasePin{}, false
}

// distributionReleaseOf is the distribution version is a release of, RKE2 or k3s, or nil. Talos
// releases are not among them: nodes of a cluster install no Talos release from these pins, and an
// administrator pins none.
func distributionReleaseOf(version string) *distributionRelease {
	for _, r := range []*distributionRelease{rke2Release, k3sRelease} {
		if r.version.MatchString(version) {
			return r
		}
	}
	return nil
}

// pinnedReleaseOf is the distribution version is a pinned release of, Talos included, or nil.
func pinnedReleaseOf(version string) *distributionRelease {
	if r := distributionReleaseOf(version); r != nil {
		return r
	}
	if talosRelease.holds(version) {
		return talosRelease
	}
	return nil
}

// talosRelease is Sidero's Talos. It is no entry of releaseTemplates, as no node installs it from
// a pin: the template bake reads the pin's images.
var talosRelease = &distributionRelease{templates.Talos, "siderolabs/talos", regexp.MustCompile(`^v1\.\d+\.\d+$`), nil}

// holds reports whether version is a release of r. A Talos release also has to be a plain
// SemVer, which refuses a part with a leading zero.
func (r *distributionRelease) holds(version string) bool {
	if !r.version.MatchString(version) {
		return false
	}
	if r == talosRelease {
		_, err := relver.ParseVersion(version)
		return err == nil
	}
	return true
}

// Format writes the pins after header, the leading comments of the file they were read from:
// Talos's releases, then RKE2's, then k3s's, each oldest first.
func (p ReleasePins) Format(header string) string {
	var b strings.Builder
	b.WriteString(header)
	versions := slices.SortedFunc(maps.Keys(p), func(a, c string) int {
		if d := cmp.Compare(pinnedReleaseOf(a).repository, pinnedReleaseOf(c).repository); d != 0 {
			return -d
		}
		va, _ := parseKubeVersion(a)
		vc, _ := parseKubeVersion(c)
		switch {
		case va.less(vc):
			return -1
		case vc.less(va):
			return 1
		}
		return cmp.Compare(a, c)
	})
	for _, v := range versions {
		if t := p[v].Talos; talosRelease.holds(v) {
			fmt.Fprintf(&b, "%s %s %s %s %s\n", v, t.Imager, t.InstallerBase, t.Extensions, t.GuestAgent)
			continue
		}
		fmt.Fprintf(&b, "%s %s %s\n", v, p[v].File, p[v].Script)
	}
	return b.String()
}

// PinsHeader is the leading comment lines of a pins file.
func PinsHeader(file string) string {
	var b strings.Builder
	for line := range strings.Lines(file) {
		if !strings.HasPrefix(line, "#") {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// errUnlisted is a release whose checksum file misses a file its nodes install.
var errUnlisted = errors.New("unlisted")

// ReleasePinner pins the RKE2, k3s and Talos releases published since, of the minor versions a
// cluster may run.
type ReleasePinner struct {
	HTTP *http.Client
	// API is GitHub's REST API, and Token, when set, a token it is read with, for a higher rate limit.
	// The token goes to the API only, never to the registry.
	API, Token string
	// Registry is Sidero's image registry, ghcr.io, read with no credentials.
	Registry string
	// Checksums reads each release's checksum file and install script.
	Checksums *ReleaseChecksums
	// retryWait replaces the first wait of a registry read's retries, so that a test does not wait.
	retryWait time.Duration
	// tokens are the registry's anonymous tokens of the run in progress, by repository.
	tokens map[string]string
}

// githubReleases bounds the pages of releases read, a hundred each. RKE2 and k3s publish a few
// hundred over the minors a channel lists.
const githubReleases = 20

// Pin adds the releases pins lacks, and returns their versions. A RKE2 or k3s release that lacks a
// file its nodes install is left out, as no node could install it. A Talos release's images are
// pushed before GitHub publishes it, so a missing one is an error.
func (p *ReleasePinner) Pin(ctx context.Context, pins ReleasePins) ([]string, error) {
	p.tokens = map[string]string{}
	var added []string
	for _, release := range []*distributionRelease{rke2Release, k3sRelease, talosRelease} {
		tags, err := p.tags(ctx, release)
		if err != nil {
			return added, err
		}
		minors := pinnedMinors(release, tags)
		for _, tag := range tags {
			v, ok := parseKubeVersion(tag)
			if _, pinned := pins[tag]; pinned || !ok || !release.holds(tag) || !minors[v.Minor] {
				continue
			}
			pin, captureErr := p.capture(ctx, release, tag)
			if errors.Is(captureErr, errUnlisted) {
				continue
			}
			if captureErr != nil {
				return added, captureErr
			}
			pins[tag] = pin
			added = append(added, tag)
		}
	}
	return added, nil
}

// capture reads a release's pin as it is now.
func (p *ReleasePinner) capture(ctx context.Context, release *distributionRelease, tag string) (ReleasePin, error) {
	if release != talosRelease {
		return p.Checksums.read(ctx, release, tag)
	}
	images, err := p.talosImages(ctx, tag, tag, tag, tag)
	return ReleasePin{Talos: images}, err
}

// pinnedMinors are the minor versions a new cluster may start on, counted down from the newest
// release's, and the one before them, which a cluster still on it upgrades from.
func pinnedMinors(release *distributionRelease, tags []string) map[int]bool {
	newest := -1
	for _, tag := range tags {
		if v, ok := parseKubeVersion(tag); ok && release.holds(tag) {
			newest = max(newest, v.Minor)
		}
	}
	minors := map[int]bool{}
	for m := newest - supportedMinors; newest >= 0 && m <= newest; m++ {
		minors[m] = true
	}
	return minors
}

// tags are the release's published releases, drafts and pre-releases left out.
func (p *ReleasePinner) tags(ctx context.Context, release *distributionRelease) ([]string, error) {
	var tags []string
	for page := 1; page <= githubReleases; page++ {
		url := p.API + "/repos/" + release.repository + "/releases?per_page=100&page=" + strconv.Itoa(page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", url, err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if p.Token != "" {
			req.Header.Set("Authorization", "Bearer "+p.Token)
		}
		resp, err := p.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", url, err)
		}
		var body []struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", url, resp.Status)
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", url, err)
		}
		if len(body) == 0 {
			return tags, nil
		}
		for _, r := range body {
			if !r.Draft && !r.Prerelease {
				tags = append(tags, r.Tag)
			}
		}
	}
	return nil, fmt.Errorf("%s publishes more than %d pages of releases", release.repository, githubReleases)
}

// pinned reports whether the pins hold version.
func (p ReleasePins) pinned(version string) bool {
	_, ok := p[version]
	return ok
}

// offered are the newest pinned release of each of the distribution's minor versions, newest first.
func (p ReleasePins) offered(release *distributionRelease) []string {
	minors := map[int]bool{}
	for v := range p {
		if kv, ok := parseKubeVersion(v); ok && release.holds(v) {
			minors[kv.Minor] = true
		}
	}
	var out []string
	for _, m := range slices.Backward(slices.Sorted(maps.Keys(minors))) {
		if v, ok := p.newest(release, m); ok {
			out = append(out, v.Raw)
		}
	}
	return out
}

// newest is the newest pinned release of the distribution's minor version, if any.
func (p ReleasePins) newest(release *distributionRelease, minor int) (kubeVersion, bool) {
	var best kubeVersion
	found := false
	for v := range p {
		kv, ok := parseKubeVersion(v)
		if !ok || kv.Minor != minor || !release.holds(v) {
			continue
		}
		if !found || best.less(kv) {
			best, found = kv, true
		}
	}
	return best, found
}
