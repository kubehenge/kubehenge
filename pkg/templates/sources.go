package templates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// Resolved is an image ready for Proxmox VE's download-url call.
type Resolved struct {
	URL string
	// FileName is the name in the import storage. Proxmox VE checks the format from its extension.
	FileName string
	// Checksum is empty when the publisher offers none; the download then relies on TLS alone.
	Checksum          string
	ChecksumAlgorithm string
}

// TalosSchematic is the Image Factory customisation of every Talos image Kubehenge uses: the QEMU
// guest agent, which Cluster API's Proxmox VE provider waits for.
const TalosSchematic = `customization:
  systemExtensions:
    officialExtensions:
      - siderolabs/qemu-guest-agent
`

// KubernetesPackagesKeyFingerprint signs pkgs.k8s.io. The key is fetched at build time because
// its expiry is extended in place; only this fingerprint is trusted.
const KubernetesPackagesKeyFingerprint = "DE15B14486CD377B9E876E1A234654DA9A296436"

const (
	// sourcesTimeout bounds each request for metadata; the images themselves Proxmox VE downloads.
	sourcesTimeout = 2 * time.Minute
	// maxResponseBytes is far above any checksum list, key or schematic answer.
	maxResponseBytes = 16 << 20
)

// Sources fetches image metadata and repository keys from the publishers.
type Sources struct {
	HTTP            *http.Client
	TalosFactoryURL string
	// Mirrors serve a publisher's files from another place, by the publisher's address
	// (https://host); a test points them at its own server.
	Mirrors map[string]string
}

// DefaultSources uses the public Talos Image Factory and every publisher's own address.
func DefaultSources() *Sources {
	return &Sources{
		HTTP:            &http.Client{Timeout: sourcesTimeout},
		TalosFactoryURL: "https://factory.talos.dev",
	}
}

// Resolve finds the URL, file name and checksum of an image.
func (s *Sources) Resolve(ctx context.Context, ref ImageRef) (Resolved, error) {
	switch ref.Kind {
	case TalosFactory:
		return s.talos(ctx, ref.Version)
	case NodeOSImage:
		f, r, err := Available(ref.NodeOS)
		if err != nil {
			return Resolved{}, err
		}
		resolved, err := f.Image(ctx, s, r)
		if err != nil {
			return Resolved{}, fmt.Errorf("the image of %s %s: %w", f.Title(), r.Name, err)
		}
		return resolved, nil
	}
	return Resolved{}, fmt.Errorf("unknown image kind %q", ref.Kind)
}

// RepositoryKey fetches a package repository's signing key and returns it ASCII-armoured after
// checking that it is one key with one of the fingerprints k allows.
func (s *Sources) RepositoryKey(ctx context.Context, k RepositoryKey) (string, error) {
	key, err := s.get(ctx, k.URL)
	if err != nil {
		return "", err
	}
	e, err := oneKey(key)
	if err != nil {
		return "", fmt.Errorf("reading the key of %s: %w", k.Name, err)
	}
	if !slices.Contains(k.Fingerprints, fingerprint(e)) {
		return "", lasting(fmt.Errorf("%s serves a key other than %s", k.URL, strings.Join(k.Fingerprints, " or ")))
	}
	// Written out again from what was checked, so that nothing else the file held reaches the
	// node's package manager.
	var out bytes.Buffer
	w, err := armor.Encode(&out, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", fmt.Errorf("writing the key of %s: %w", k.Name, err)
	}
	if err = e.Serialize(w); err != nil {
		return "", fmt.Errorf("writing the key of %s: %w", k.Name, err)
	}
	if err = w.Close(); err != nil {
		return "", fmt.Errorf("writing the key of %s: %w", k.Name, err)
	}
	out.WriteString("\n")
	return out.String(), nil
}

// oneKey reads an armoured file that holds exactly one public key. The keyring reader stops at the
// end of the first armoured block, so a second block after it is refused here.
func oneKey(armoured []byte) (*openpgp.Entity, error) {
	if n := bytes.Count(armoured, []byte("-----BEGIN PGP ")); n != 1 {
		return nil, fmt.Errorf("%d armoured blocks, not one", n)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armoured))
	if err != nil {
		return nil, err //nolint:wrapcheck // the callers say whose key it is
	}
	if len(keyring) != 1 {
		return nil, fmt.Errorf("%d keys, not one", len(keyring))
	}
	return keyring[0], nil
}

// TalosSchematicID registers TalosSchematic with the Image Factory, which names it by its content,
// the same ID on every call.
func (s *Sources) TalosSchematicID(ctx context.Context) (string, error) {
	body, err := s.do(ctx, http.MethodPost, s.TalosFactoryURL+"/schematics", strings.NewReader(TalosSchematic))
	if err != nil {
		return "", fmt.Errorf("registering the Talos schematic: %w", err)
	}
	var schematic struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &schematic); err != nil || len(schematic.ID) != hex.EncodedLen(sha256.Size) {
		return "", fmt.Errorf("unexpected Image Factory answer %q", body)
	}
	return schematic.ID, nil
}

// TalosInstaller is the installer image a node of TalosSchematic upgrades to a Talos release with,
// from the Image Factory's registry, which signs it.
func (s *Sources) TalosInstaller(ctx context.Context, version string) (string, error) {
	if !versionRE.MatchString(version) {
		return "", fmt.Errorf("talos version %q is not of the form v1.2.3", version)
	}
	id, err := s.TalosSchematicID(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(s.TalosFactoryURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("image factory %q: no host", s.TalosFactoryURL)
	}
	return fmt.Sprintf("%s/nocloud-installer/%s:%s", u.Host, id, version), nil
}

// TalosReleases are the Talos releases the Image Factory builds, pre-releases left out.
func (s *Sources) TalosReleases(ctx context.Context) ([]string, error) {
	body, err := s.do(ctx, http.MethodGet, s.TalosFactoryURL+"/versions", nil)
	if err != nil {
		return nil, fmt.Errorf("listing the Talos releases: %w", err)
	}
	var all []string
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("unexpected Image Factory answer %q", body)
	}
	out := make([]string, 0, len(all))
	for _, v := range all {
		if versionRE.MatchString(v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// at is where a publisher's URL is fetched from: the publisher itself unless a mirror stands in.
func (s *Sources) at(address string) string {
	for publisher, mirror := range s.Mirrors {
		if rest, ok := strings.CutPrefix(address, publisher); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			return mirror + rest
		}
	}
	return address
}

func (s *Sources) get(ctx context.Context, address string) ([]byte, error) {
	return s.do(ctx, http.MethodGet, s.at(address), nil)
}

func (s *Sources) talos(ctx context.Context, version string) (Resolved, error) {
	id, err := s.TalosSchematicID(ctx)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		// The uncompressed qcow2: Proxmox VE cannot decompress images it imports.
		URL:      fmt.Sprintf("%s/image/%s/%s/nocloud-amd64.qcow2", s.TalosFactoryURL, id, version),
		FileName: fmt.Sprintf("template-talos-%s-%s-nocloud-amd64.qcow2", version, id[:12]),
	}, nil
}

// signedImage resolves an image whose publisher lists its SHA-256 in a checksum file it signs with
// a detached signature at sigURL: the signature is checked against the embedded key, which must be
// the one with the pinned fingerprint, before the checksum is read. The template's disk is
// downloaded under fileName.
func (s *Sources) signedImage(ctx context.Context, signer signingKey, imageURL, sumsURL, sigURL, fileName string) (Resolved, error) {
	sums, err := s.get(ctx, sumsURL)
	if err != nil {
		return Resolved{}, err
	}
	sig, err := s.get(ctx, sigURL)
	if err != nil {
		return Resolved{}, err
	}
	if err = signer.verify(sums, sig); err != nil {
		return Resolved{}, fmt.Errorf("%s: %w", sumsURL, err)
	}
	file := imageURL[strings.LastIndex(imageURL, "/")+1:]
	sum, err := findChecksum(sums, file)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		URL:               s.at(imageURL),
		FileName:          fmt.Sprintf("template-%s-%s.qcow2", fileName, sum[:12]),
		Checksum:          sum,
		ChecksumAlgorithm: "sha256",
	}, nil
}

// signingKey is a publisher's embedded key and the fingerprint it must have.
type signingKey struct {
	name        string
	armoured    []byte
	fingerprint string
}

// verify checks a detached signature, armoured or binary, over data.
func (k signingKey) verify(data, sig []byte) error {
	key, err := oneKey(k.armoured)
	if err != nil {
		return fmt.Errorf("reading the embedded %s key: %w", k.name, err)
	}
	if fingerprint(key) != k.fingerprint {
		return fmt.Errorf("the embedded %s key is not %s", k.name, k.fingerprint)
	}
	keyring := openpgp.EntityList{key}
	v3, isV3, err := parseV3(sig)
	switch {
	case err != nil:
		return fmt.Errorf("not signed by %s's key: %w", k.name, err)
	case isV3:
		if err = v3.verify(keyring[0], data, time.Now()); err != nil {
			return fmt.Errorf("not signed by %s's key: %w", k.name, err)
		}
		return nil
	}
	check := openpgp.CheckDetachedSignature
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN")) {
		check = openpgp.CheckArmoredDetachedSignature
	}
	signer, err := check(keyring, bytes.NewReader(data), bytes.NewReader(sig), nil)
	if err != nil {
		return fmt.Errorf("not signed by %s's key: %w", k.name, err)
	}
	if fp := fingerprint(signer); fp != k.fingerprint {
		return fmt.Errorf("signed by %s, not %s's %s", fp, k.name, k.fingerprint)
	}
	return nil
}

// The lines of a checksum list: the BSD form, as Rocky Linux writes them, SHA256 (file) = hex, and
// the GNU one, hex and the file, marked * when read as binary.
var (
	bsdChecksum = regexp.MustCompile(`^SHA256 \((?P<name>.+)\) = (?P<sum>[0-9A-Fa-f]+)$`)
	gnuChecksum = regexp.MustCompile(`^(?P<sum>[0-9A-Fa-f]+)\s+\*?(?P<name>\S.*)$`)
)

// findChecksum reads a file's SHA-256 from a checksum list, in the GNU form or the BSD one.
func findChecksum(sums []byte, file string) (string, error) {
	for line := range strings.SplitSeq(string(sums), "\n") {
		line = strings.TrimSpace(line)
		var name, sum string
		for _, form := range []*regexp.Regexp{bsdChecksum, gnuChecksum} {
			if m := form.FindStringSubmatch(line); m != nil {
				name, sum = m[form.SubexpIndex("name")], m[form.SubexpIndex("sum")]
				break
			}
		}
		if name != file {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil || len(sum) != hex.EncodedLen(sha256.Size) {
			return "", fmt.Errorf("malformed checksum for %s", file)
		}
		return strings.ToLower(sum), nil
	}
	return "", fmt.Errorf("%s is not listed in its checksum file", file)
}

func fingerprint(e *openpgp.Entity) string {
	return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
}

func (s *Sources) do(ctx context.Context, method, address string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, address, err)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, address, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the answer: %w", method, address, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s %s: %s", method, address, resp.Status)
	}
	return data, nil
}
