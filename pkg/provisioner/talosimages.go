package provisioner

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// registryAttempts is how often a registry exchange is tried in all, and registryFirstWait the
	// wait before the second try, doubled before each further one.
	registryAttempts  = 4
	registryFirstWait = 2 * time.Second
	// registryBodyLimit caps every body read, a layer's bytes after gunzip included. The live ones
	// are a few KB.
	registryBodyLimit = 1 << 20
	// registryTarLimit caps the tar stream a layer unpacks to, which a gzip bomb would otherwise fill.
	registryTarLimit = 16 << 20

	talosVersionLabel = "alpha.talos.dev/version"
	imageDigestsFile  = "image-digests"
)

const (
	ociIndex       = "application/vnd.oci.image.index.v1+json"
	dockerIndex    = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifest    = "application/vnd.oci.image.manifest.v1+json"
	dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// registryAccept names every form Sidero publishes an index or a manifest in.
var registryAccept = strings.Join([]string{ociIndex, dockerIndex, ociManifest, dockerManifest}, ", ")

var (
	bearerField   = regexp.MustCompile(`(\w+)="([^"]*)"`)
	versionSuffix = regexp.MustCompile(`-(v\d+\.\d+\.\d+)@sha256:`)
)

// talosImage is one of the images of a Talos release, as an error names it.
type talosImage struct{ release, name, repository string }

// notServedError is a 404 for a reference.
type notServedError struct {
	img       talosImage
	reference string
}

func (e notServedError) Error() string {
	return fmt.Sprintf("%s %s %s: 404 Not Found", e.img.release, e.img.name, e.reference)
}

// notOf says what a body that is no index or manifest is, with the parse error when there was one.
func notOf(kind string, err error) error {
	if err != nil {
		return fmt.Errorf("not an image %s: %w", kind, err)
	}
	return fmt.Errorf("not an image %s: its mediaType is neither of the forms Sidero publishes", kind)
}

// descriptor is an entry of an index or a manifest.
type descriptor struct {
	Digest   string    `json:"digest"`
	Platform *platform `json:"platform"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// imageConfig is the part of an image's config blob that names its release.
type imageConfig struct {
	Config configLabels `json:"config"`
}

type configLabels struct {
	Labels map[string]string `json:"Labels"`
}

// manifestDocument is an index or a manifest, whichever its mediaType says.
type manifestDocument struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Layers    []descriptor `json:"layers"`
	Config    descriptor   `json:"config"`
}

// registryAnswer is what one exchange got that is no failure to try again.
type registryAnswer struct {
	status    int
	challenge string
	body      []byte
	over      bool
}

// talosImages reads the imager, the installer base and the extensions image of a Talos release,
// each by a reference that is the release's tag or a pinned digest, and the guest agent the
// extensions image names. It fails unless the images name the release themselves.
func (p *ReleasePinner) talosImages(ctx context.Context, release, imager, installerBase, extensions string) (templates.TalosPin, error) {
	var pin templates.TalosPin
	var err error
	if pin.Imager, err = p.labelledImage(ctx, talosImage{release, "imager", "imager"}, imager); err != nil {
		return templates.TalosPin{}, err
	}
	if pin.InstallerBase, err = p.labelledImage(ctx, talosImage{release, "installer base", "installer-base"}, installerBase); err != nil {
		return templates.TalosPin{}, err
	}
	if pin.Extensions, pin.GuestAgent, err = p.extensionsImage(ctx, talosImage{release, "extensions image", "extensions"}, extensions); err != nil {
		return templates.TalosPin{}, err
	}
	return pin, nil
}

// labelledImage reads an image that carries its release in the label alpha.talos.dev/version, and
// returns the digest of its index.
func (p *ReleasePinner) labelledImage(ctx context.Context, img talosImage, reference string) (string, error) {
	digest, manifest, err := p.amd64Manifest(ctx, img, reference)
	if err != nil {
		return "", err
	}
	raw, err := p.blob(ctx, img, manifest.Config.Digest)
	if err != nil {
		return "", err
	}
	var config imageConfig
	if err = json.Unmarshal(raw, &config); err != nil {
		return "", fmt.Errorf("%s %s: config %s: %w", img.release, img.name, manifest.Config.Digest, err)
	}
	if got := config.Config.Labels[talosVersionLabel]; got != img.release {
		return "", fmt.Errorf("%s %s: its %s is %q, not the release", img.release, img.name, talosVersionLabel, got)
	}
	return digest, nil
}

// extensionsImage reads the extensions image, which has no label: its release is the one whose
// own images its image-digests names most. It returns the image's digest and the guest agent.
func (p *ReleasePinner) extensionsImage(ctx context.Context, img talosImage, reference string) (string, string, error) {
	digest, manifest, err := p.amd64Manifest(ctx, img, reference)
	if err != nil {
		return "", "", err
	}
	var digests []byte
	for _, layer := range manifest.Layers {
		var raw []byte
		if raw, err = p.blob(ctx, img, layer.Digest); err != nil {
			return "", "", err
		}
		if digests, err = imageDigestsOf(img, layer.Digest, raw); err != nil {
			return "", "", err
		}
		if digests != nil {
			break
		}
	}
	if digests == nil {
		return "", "", fmt.Errorf("%s %s: no %s in any layer", img.release, img.name, imageDigestsFile)
	}
	if err = checkSuffixes(img, string(digests)); err != nil {
		return "", "", err
	}
	agent, err := guestAgent(img, string(digests))
	if err != nil {
		return "", "", err
	}
	return digest, agent, nil
}

// imageDigestsOf returns the file image-digests of a gzipped tar layer, or nil when it holds none.
func imageDigestsOf(img talosImage, digest string, layer []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		return nil, fmt.Errorf("%s %s %s: %w", img.release, img.name, digest, err)
	}
	tr := tar.NewReader(io.LimitReader(gz, registryTarLimit))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s %s %s: reading the layer: %w", img.release, img.name, digest, err)
		}
		// A tar written with "./" names the same file, as the appliance's unpacking reads it.
		if h.Typeflag != tar.TypeReg || path.Clean(strings.TrimPrefix(h.Name, "./")) != imageDigestsFile {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, registryBodyLimit+1))
		if err != nil {
			return nil, fmt.Errorf("%s %s %s: reading %s: %w", img.release, img.name, digest, imageDigestsFile, err)
		}
		if len(body) > registryBodyLimit {
			return nil, fmt.Errorf("%s %s %s: %s larger than 1 MiB", img.release, img.name, digest, imageDigestsFile)
		}
		return body, nil
	}
}

// checkSuffixes ties the extensions image to its release by its content. Its own images are tagged
// <version>-<release>, and the release's suffix has to be on more lines than any other, as the
// NVIDIA container toolkit's two images carry the toolkit's own version in that form.
func checkSuffixes(img talosImage, digests string) error {
	counts := map[string]int{}
	for line := range strings.Lines(digests) {
		if m := versionSuffix.FindStringSubmatch(line); m != nil {
			counts[m[1]]++
		}
	}
	other, otherCount := "", 0
	for _, suffix := range slices.Sorted(maps.Keys(counts)) {
		if suffix != img.release && counts[suffix] > otherCount {
			other, otherCount = suffix, counts[suffix]
		}
	}
	own := counts[img.release]
	if own == 0 || own <= otherCount {
		return fmt.Errorf("%s %s: %s names -%s on %d lines and -%s on %d, so it is not this release's", img.release, img.name, imageDigestsFile, img.release, own, other, otherCount)
	}
	return nil
}

// guestAgent reads the one QEMU guest agent line of image-digests, as <tag>@sha256:<digest>.
func guestAgent(img talosImage, digests string) (string, error) {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(digests))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); strings.HasPrefix(line, talosGuestAgent) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		return "", fmt.Errorf("%s %s: %s has %d lines of the QEMU guest agent, want one", img.release, img.name, imageDigestsFile, len(lines))
	}
	image, err := ParseImage(lines[0])
	if err != nil || image.Registry != "ghcr.io" || image.Repository != "siderolabs/qemu-guest-agent" {
		return "", fmt.Errorf("%s %s: the guest agent line %q is not ghcr.io/siderolabs/qemu-guest-agent:<tag>@sha256:<digest>", img.release, img.name, lines[0])
	}
	return image.Tag + "@sha256:" + image.Digest, nil
}

// amd64Manifest reads an image's index by reference, then its one linux/amd64 manifest, and returns
// the digest of the index as the bytes read hash to.
func (p *ReleasePinner) amd64Manifest(ctx context.Context, img talosImage, reference string) (string, manifestDocument, error) {
	body, err := p.manifest(ctx, img, reference)
	if err != nil {
		return "", manifestDocument{}, err
	}
	digest := "sha256:" + hexSHA256(body)
	var index manifestDocument
	if err = json.Unmarshal(body, &index); err != nil || (index.MediaType != ociIndex && index.MediaType != dockerIndex) {
		return "", manifestDocument{}, fmt.Errorf("%s %s %s: %w", img.release, img.name, reference, notOf("index", err))
	}
	var amd64 []descriptor
	for _, m := range index.Manifests {
		if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			amd64 = append(amd64, m)
		}
	}
	if len(amd64) != 1 {
		return "", manifestDocument{}, fmt.Errorf("%s %s: %d linux/amd64 manifests, want one", img.release, img.name, len(amd64))
	}
	raw, err := p.manifest(ctx, img, amd64[0].Digest)
	if err != nil {
		return "", manifestDocument{}, err
	}
	var manifest manifestDocument
	if err = json.Unmarshal(raw, &manifest); err != nil || (manifest.MediaType != ociManifest && manifest.MediaType != dockerManifest) {
		return "", manifestDocument{}, fmt.Errorf("%s %s %s: %w", img.release, img.name, amd64[0].Digest, notOf("manifest", err))
	}
	return digest, manifest, nil
}

// manifest reads an index or manifest by a tag or a digest. By a digest it refuses bytes that do
// not hash to it.
func (p *ReleasePinner) manifest(ctx context.Context, img talosImage, reference string) ([]byte, error) {
	return p.registryGet(ctx, img, "manifests", reference, registryAccept)
}

// blob reads a config or layer by its digest.
func (p *ReleasePinner) blob(ctx context.Context, img talosImage, digest string) ([]byte, error) {
	return p.registryGet(ctx, img, "blobs", digest, "")
}

// registryGet reads a manifest or blob. A reference with a colon is a digest, which has to be a
// SHA-256 one so that the bytes can be checked against it. Any other is refused before it reaches
// an address.
func (p *ReleasePinner) registryGet(ctx context.Context, img talosImage, kind, reference, accept string) ([]byte, error) {
	if algorithm, hex, isDigest := strings.Cut(reference, ":"); isDigest && (algorithm != "sha256" || !sha256Hex.MatchString(hex)) {
		return nil, fmt.Errorf("%s %s: %q is not a SHA-256 digest", img.release, img.name, reference)
	}
	address := p.Registry + "/v2/siderolabs/" + img.repository + "/" + kind + "/" + url.PathEscape(reference)
	ans, err := p.authorised(ctx, img, address, accept)
	if err != nil {
		return nil, err
	}
	switch {
	case ans.status == http.StatusNotFound:
		return nil, notServedError{img: img, reference: reference}
	case ans.status != http.StatusOK:
		return nil, fmt.Errorf("%s %s: %s: %d", img.release, img.name, address, ans.status)
	case ans.over:
		return nil, fmt.Errorf("%s %s %s: larger than 1 MiB", img.release, img.name, reference)
	}
	if want, ok := strings.CutPrefix(reference, "sha256:"); ok && hexSHA256(ans.body) != want {
		return nil, fmt.Errorf("%s %s %s: the bytes served hash to sha256:%s", img.release, img.name, reference, hexSHA256(ans.body))
	}
	return ans.body, nil
}

// authorised sends a request with the repository's anonymous token. A 401 fetches a token from the
// challenge and sends the request again. A 401 to a token already held gets one fresh token, and a
// second one fails.
func (p *ReleasePinner) authorised(ctx context.Context, img talosImage, address, accept string) (registryAnswer, error) {
	if p.tokens == nil {
		p.tokens = map[string]string{}
	}
	renewed := false
	for {
		held := p.tokens[img.repository]
		ans, err := p.exchange(ctx, img, address, accept, held)
		if err != nil || ans.status != http.StatusUnauthorized {
			return ans, err
		}
		if held != "" && renewed {
			return registryAnswer{}, fmt.Errorf("%s %s: %s: 401 with a fresh token", img.release, img.name, address)
		}
		renewed = held != ""
		if p.tokens[img.repository], err = p.fetchToken(ctx, img, ans.challenge); err != nil {
			return registryAnswer{}, err
		}
	}
}

// exchange is one request, tried again as far as its failure may pass. A 401 is no such failure.
func (p *ReleasePinner) exchange(ctx context.Context, img talosImage, address, accept, token string) (registryAnswer, error) {
	var ans registryAnswer
	err := templates.Retrying(ctx, registryAttempts, p.firstWait(), func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
		if err != nil {
			return fmt.Errorf("%s %s: %w", img.release, img.name, err)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := p.HTTP.Do(req)
		if err != nil {
			return templates.Transient(fmt.Errorf("%s %s: %s: %w", img.release, img.name, address, err))
		}
		defer func() { _ = resp.Body.Close() }()
		if templates.TransientStatus(resp.StatusCode) {
			return templates.Transient(fmt.Errorf("%s %s: %s: %s", img.release, img.name, address, resp.Status))
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, registryBodyLimit+1))
		if err != nil {
			return templates.Transient(fmt.Errorf("%s %s: %s: reading the answer: %w", img.release, img.name, address, err))
		}
		ans = registryAnswer{status: resp.StatusCode, challenge: resp.Header.Get("WWW-Authenticate"), over: len(body) > registryBodyLimit}
		if !ans.over {
			ans.body = body
		}
		return nil
	})
	return ans, err //nolint:wrapcheck // try's own error, which names the release, the image and the address
}

// fetchToken gets the anonymous token a registry's Bearer challenge names. It sends no credentials.
func (p *ReleasePinner) fetchToken(ctx context.Context, img talosImage, challenge string) (string, error) {
	fields := map[string]string{}
	for _, m := range bearerField.FindAllStringSubmatch(challenge, -1) {
		fields[m[1]] = m[2]
	}
	realm, err := url.Parse(fields["realm"])
	if !strings.HasPrefix(challenge, "Bearer ") || err != nil || realm.Host == "" {
		return "", fmt.Errorf("%s %s: no Bearer challenge in %q", img.release, img.name, challenge)
	}
	query := realm.Query()
	query.Set("service", fields["service"])
	query.Set("scope", fields["scope"])
	realm.RawQuery = query.Encode()
	var token string
	err = templates.Retrying(ctx, registryAttempts, p.firstWait(), func() error {
		var tryErr error
		token, tryErr = p.tokenOnce(ctx, img, realm.String())
		return tryErr
	})
	return token, err //nolint:wrapcheck // try's own error, which names the release, the image and the address
}

// tokenOnce asks the token service once. A failure that may pass is marked Transient.
func (p *ReleasePinner) tokenOnce(ctx context.Context, img talosImage, address string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", img.release, img.name, err)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return "", templates.Transient(fmt.Errorf("%s %s: %s: %w", img.release, img.name, address, err)) //nolint:wrapcheck // Transient marks the error it is given
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("%s %s: %s: %s", img.release, img.name, address, resp.Status)
		if templates.TransientStatus(resp.StatusCode) {
			return "", templates.Transient(err) //nolint:wrapcheck // Transient marks the error it is given
		}
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, registryBodyLimit))
	if err != nil {
		return "", templates.Transient(fmt.Errorf("%s %s: %s: reading the answer: %w", img.release, img.name, address, err)) //nolint:wrapcheck // Transient marks the error it is given
	}
	var answer struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(body, &answer); err != nil || answer.Token == "" {
		return "", fmt.Errorf("%s %s: %s answered no token", img.release, img.name, address)
	}
	return answer.Token, nil
}

func (p *ReleasePinner) firstWait() time.Duration {
	if p.retryWait > 0 {
		return p.retryWait
	}
	return registryFirstWait
}

// CheckTalos reads every Talos pin by its digests, oldest first, and fails unless each image is
// still served, names its release and holds the guest agent the pin has. It reads no tag and calls
// no GitHub. It checks no signature: scripts/verify-talos-pins.sh does.
func (p *ReleasePinner) CheckTalos(ctx context.Context, pins ReleasePins) error {
	p.tokens = map[string]string{}
	talos := pins.Talos()
	versions := slices.SortedFunc(maps.Keys(talos), func(a, b string) int {
		va, _ := parseKubeVersion(a)
		vb, _ := parseKubeVersion(b)
		switch {
		case va.less(vb):
			return -1
		case vb.less(va):
			return 1
		}
		return 0
	})
	for _, release := range versions {
		pin := talos[release]
		got, err := p.talosImages(ctx, release, pin.Imager, pin.InstallerBase, pin.Extensions)
		if gone, ok := errors.AsType[notServedError](err); ok {
			return fmt.Errorf("Talos %s: Sidero no longer serves its %s %s: delete the line and run make release-pins", release, gone.img.name, gone.reference)
		}
		if err != nil {
			return err
		}
		if got.GuestAgent != pin.GuestAgent {
			return fmt.Errorf("%s extensions image: the pin has the guest agent %s, its image-digests names %s", release, pin.GuestAgent, got.GuestAgent)
		}
	}
	return nil
}
