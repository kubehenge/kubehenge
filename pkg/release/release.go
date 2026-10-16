// Package release is what Kubehenge's appliance knows of its own releases: the version it is, the
// signed index of published releases, and which of them an upgrade may go to.
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is this build's release, set at link time (-X); "dev" for anything built otherwise.
var Version = "dev"

// publicKey verifies the release index: an Ed25519 public key, base64, the only one this build
// trusts, set at link time (-X) from release.pub. Empty in a build without one, which then
// upgrades nothing. Never configurable at run time: whoever could change it could sign upgrades.
var publicKey = ""

// PublicKey returns the key the index has to be signed with, or nil.
func PublicKey() ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return raw
}

// Index lists the published releases.
type Index struct {
	Releases []Release `json:"releases"`
}

// Release is one published release and its appliance archive.
type Release struct {
	Version string `json:"version"`
	// URL of the linux/amd64 archive, and its SHA-256 in hex.
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	// Tofu is the OpenTofu release the appliance runs the management-cluster module with.
	Tofu *Artefact `json:"tofu,omitempty"`
}

// Artefact is a download pinned by its checksum.
type Artefact struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// ErrSignature is an index whose signature does not verify.
var ErrSignature = errors.New("the release index is not signed by Kubehenge's release key")

// Sign signs an index's bytes; the signature travels beside it, base64.
func Sign(index []byte, key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, index))
}

// Parse verifies an index against key and decodes it. Nothing in it is trusted before the
// signature is checked.
func Parse(index []byte, signature string, key ed25519.PublicKey) (*Index, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, errors.New("this build carries no release key, so it upgrades nothing")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil || !ed25519.Verify(key, index, sig) {
		return nil, ErrSignature
	}
	var ix Index
	if err := json.Unmarshal(index, &ix); err != nil {
		return nil, fmt.Errorf("release index: %w", err)
	}
	for _, r := range ix.Releases {
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("release index: %w", err)
		}
	}
	return &ix, nil
}

func (r Release) validate() error {
	if _, err := ParseVersion(r.Version); err != nil {
		return err
	}
	if !strings.HasPrefix(r.URL, "https://") || !isSHA256(r.SHA256) {
		return fmt.Errorf("%s needs an https URL and a SHA-256", r.Version)
	}
	if t := r.Tofu; t != nil && (!strings.HasPrefix(t.URL, "https://") || !isSHA256(t.SHA256) || t.Version == "") {
		return fmt.Errorf("%s names OpenTofu without a version, an https URL and a SHA-256", r.Version)
	}
	return nil
}

func isSHA256(s string) bool {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Add appends a release newer than every one in the index, so a published index only ever grows
// and an upgrade never meets two releases of one version.
func (ix *Index) Add(r Release) error {
	if err := r.validate(); err != nil {
		return err
	}
	v, _ := ParseVersion(r.Version)
	for _, have := range ix.Releases {
		if h, _ := ParseVersion(have.Version); !h.Less(v) {
			return fmt.Errorf("%s is not newer than %s, already in the index", v, h)
		}
	}
	ix.Releases = append(ix.Releases, r)
	return nil
}

// Marshal writes the index as it is published and signed.
func (ix *Index) Marshal() ([]byte, error) {
	out, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the release index: %w", err)
	}
	return append(out, '\n'), nil
}

// SemVer is a release version, major.minor.patch.
type SemVer struct{ Major, Minor, Patch int }

// semVerParts are major, minor and patch.
const semVerParts = 3

// ParseVersion reads 1.2.3 or v1.2.3. Pre-releases are not offered as upgrades.
func ParseVersion(s string) (SemVer, error) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != semVerParts {
		return SemVer{}, fmt.Errorf("version %q is not major.minor.patch", s)
	}
	var v [semVerParts]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return SemVer{}, fmt.Errorf("version %q is not major.minor.patch", s)
		}
		v[i] = n
	}
	return SemVer{v[0], v[1], v[2]}, nil
}

// Less orders versions.
func (a SemVer) Less(b SemVer) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	return a.Patch < b.Patch
}

func (a SemVer) String() string { return fmt.Sprintf("%d.%d.%d", a.Major, a.Minor, a.Patch) }

// Policy is how far an automatic upgrade may go.
type Policy string

const (
	// PolicyOff upgrades nothing by itself.
	PolicyOff Policy = "off"
	// PolicyPatch upgrades within the installed minor version.
	PolicyPatch Policy = "patch"
	// PolicyMinor upgrades within the installed major version.
	PolicyMinor Policy = "minor"
)

// Newest returns the newest release above current that policy allows, or nil. A manual upgrade
// asks with PolicyMinor and may then name a newer major itself through Find.
func (ix *Index) Newest(current string, policy Policy) *Release {
	cur, err := ParseVersion(current)
	if err != nil || policy == PolicyOff {
		return nil
	}
	var best *Release
	var bestV SemVer
	for i := range ix.Releases {
		r := &ix.Releases[i]
		v, _ := ParseVersion(r.Version)
		if !cur.Less(v) || v.Major != cur.Major || (policy == PolicyPatch && v.Minor != cur.Minor) {
			continue
		}
		if best == nil || bestV.Less(v) {
			best, bestV = r, v
		}
	}
	return best
}

// Find returns the release version names if it is newer than current; an upgrade never goes
// back, so an old signed release cannot be replayed onto the appliance.
func (ix *Index) Find(current, version string) (*Release, error) {
	cur, err := ParseVersion(current)
	if err != nil {
		return nil, fmt.Errorf("this build, %s, is not a release and upgrades nothing", current)
	}
	want, err := ParseVersion(version)
	if err != nil {
		return nil, err
	}
	if !cur.Less(want) {
		return nil, fmt.Errorf("%s is not newer than the installed %s", want, cur)
	}
	for i := range ix.Releases {
		if v, _ := ParseVersion(ix.Releases[i].Version); v == want {
			return &ix.Releases[i], nil
		}
	}
	return nil, fmt.Errorf("%s is not a published release", want)
}
