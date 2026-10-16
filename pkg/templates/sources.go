package templates

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
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

const (
	// UbuntuSigningKeyFingerprint is the UEC Image Automatic Signing Key, which signs the checksums
	// of Ubuntu's cloud images.
	UbuntuSigningKeyFingerprint = "D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81"
	// KubernetesPackagesKeyFingerprint signs pkgs.k8s.io. The key is fetched at build time because
	// its expiry is extended in place; only this fingerprint is trusted.
	KubernetesPackagesKeyFingerprint = "DE15B14486CD377B9E876E1A234654DA9A296436"
)

const (
	// sourcesTimeout bounds each request for metadata; the images themselves Proxmox VE downloads.
	sourcesTimeout = 2 * time.Minute
	// maxResponseBytes is far above any checksum list, key or schematic answer.
	maxResponseBytes = 16 << 20
)

//go:embed keys/ubuntu-cloud-images.asc
var ubuntuSigningKey []byte

// Sources fetches image metadata from the publishers.
type Sources struct {
	HTTP            *http.Client
	TalosFactoryURL string
	UbuntuCloudURL  string
	KubernetesPkgs  string
}

// DefaultSources uses the public Talos Image Factory, Ubuntu cloud images and pkgs.k8s.io.
func DefaultSources() *Sources {
	return &Sources{
		HTTP:            &http.Client{Timeout: sourcesTimeout},
		TalosFactoryURL: "https://factory.talos.dev",
		UbuntuCloudURL:  "https://cloud-images.ubuntu.com/releases",
		KubernetesPkgs:  "https://pkgs.k8s.io",
	}
}

// Resolve finds the URL, file name and checksum of an image.
func (s *Sources) Resolve(ctx context.Context, ref ImageRef) (Resolved, error) {
	switch ref.Kind {
	case TalosFactory:
		return s.talos(ctx, ref.Version)
	case UbuntuCloud:
		return s.ubuntu(ctx, ref.Version)
	}
	return Resolved{}, fmt.Errorf("unknown image kind %q", ref.Kind)
}

// KubernetesPackagesKey fetches the pkgs.k8s.io signing key for a Kubernetes minor series, such as
// v1.37, and returns it ASCII-armoured after checking its fingerprint.
func (s *Sources) KubernetesPackagesKey(ctx context.Context, series string) (string, error) {
	key, err := s.do(ctx, http.MethodGet, fmt.Sprintf("%s/core:/stable:/%s/deb/Release.key", s.KubernetesPkgs, series), nil)
	if err != nil {
		return "", err
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key))
	if err != nil {
		return "", fmt.Errorf("reading the pkgs.k8s.io key: %w", err)
	}
	if len(keyring) != 1 || fingerprint(keyring[0]) != KubernetesPackagesKeyFingerprint {
		return "", fmt.Errorf("pkgs.k8s.io serves a key other than %s", KubernetesPackagesKeyFingerprint)
	}
	return string(key), nil
}

func (s *Sources) talos(ctx context.Context, version string) (Resolved, error) {
	body, err := s.do(ctx, http.MethodPost, s.TalosFactoryURL+"/schematics", strings.NewReader(TalosSchematic))
	if err != nil {
		return Resolved{}, fmt.Errorf("registering the Talos schematic: %w", err)
	}
	var schematic struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &schematic); err != nil || len(schematic.ID) != hex.EncodedLen(sha256.Size) {
		return Resolved{}, fmt.Errorf("unexpected Image Factory answer %q", body)
	}
	return Resolved{
		// The uncompressed qcow2: Proxmox VE cannot decompress images it imports.
		URL:      fmt.Sprintf("%s/image/%s/%s/nocloud-amd64.qcow2", s.TalosFactoryURL, schematic.ID, version),
		FileName: fmt.Sprintf("template-talos-%s-%s-nocloud-amd64.qcow2", version, schematic.ID[:12]),
	}, nil
}

func (s *Sources) ubuntu(ctx context.Context, os string) (Resolved, error) {
	codename, ok := OperatingSystems[os]
	if !ok {
		return Resolved{}, fmt.Errorf("unsupported operating system %q", os)
	}
	dir := fmt.Sprintf("%s/%s/release", s.UbuntuCloudURL, codename)
	sums, err := s.do(ctx, http.MethodGet, dir+"/SHA256SUMS", nil)
	if err != nil {
		return Resolved{}, err
	}
	sig, err := s.do(ctx, http.MethodGet, dir+"/SHA256SUMS.gpg", nil)
	if err != nil {
		return Resolved{}, err
	}
	err = verifyUbuntuChecksums(sums, sig)
	if err != nil {
		return Resolved{}, err
	}
	file := os + "-server-cloudimg-amd64.img"
	sum, err := findChecksum(sums, file)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		URL: dir + "/" + file,
		// The .img is a qcow2; Proxmox VE accepts it for import only under that extension.
		FileName:          fmt.Sprintf("template-%s-%s.qcow2", os, sum[:12]),
		Checksum:          sum,
		ChecksumAlgorithm: "sha256",
	}, nil
}

func verifyUbuntuChecksums(sums, sig []byte) error {
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(ubuntuSigningKey))
	if err != nil {
		return fmt.Errorf("reading the embedded Ubuntu signing key: %w", err)
	}
	check := openpgp.CheckDetachedSignature
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN")) {
		check = openpgp.CheckArmoredDetachedSignature
	}
	signer, err := check(keyring, bytes.NewReader(sums), bytes.NewReader(sig), nil)
	if err != nil {
		return fmt.Errorf("the cloud images' SHA256SUMS is not signed by Ubuntu's cloud image key: %w", err)
	}
	if fp := fingerprint(signer); fp != UbuntuSigningKeyFingerprint {
		return fmt.Errorf("the cloud images' SHA256SUMS is signed by %s, not %s", fp, UbuntuSigningKeyFingerprint)
	}
	return nil
}

func findChecksum(sums []byte, file string) (string, error) {
	for line := range strings.SplitSeq(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == file {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != hex.EncodedLen(sha256.Size) {
				return "", fmt.Errorf("malformed checksum for %s", file)
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s is not listed in SHA256SUMS", file)
}

func fingerprint(e *openpgp.Entity) string {
	return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
}

func (s *Sources) do(ctx context.Context, method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the answer: %w", method, url, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	return data, nil
}
