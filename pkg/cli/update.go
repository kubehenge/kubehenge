package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// DefaultIndex is where the appliance finds published releases: on the website, which signs the
// latest release's index again every week, so that it has not expired.
const DefaultIndex = "https://kubehenge.dev/releases/index.json"

const (
	// updateTimeout bounds the download of a release, the largest file the updater fetches.
	updateTimeout = 10 * time.Minute
	// healthWait is how long the restarted interface has to answer, and healthSettle how long it
	// then has to keep running, with systemd's restarts 5 seconds apart, before an upgrade counts.
	healthWait   = 2 * time.Minute
	healthSettle = 20 * time.Second
	// privateKeyMode keeps the release key to the one who made it.
	privateKeyMode os.FileMode = 0o600
	// signatureMode lets anyone read a signature, which is public.
	signatureMode os.FileMode = 0o644
)

// releaseIndexDefault lets a mirror serve the index. Whatever the source, only an index signed by
// the release key is used.
func releaseIndexDefault() string {
	if v := os.Getenv("KUBEHENGE_RELEASE_INDEX"); v != "" {
		return v
	}
	return DefaultIndex
}

func newUpdateCommand() *cobra.Command {
	check := appliance.ServiceCheck{
		CertFile: "/etc/kubehenge/tls.crt", Address: "127.0.0.1:443", Wait: healthWait, Settle: healthSettle,
	}
	u := appliance.Updater{
		Root: "/", Restart: appliance.RestartService, Healthy: check.Healthy, Current: release.Version, Keys: release.PublicKeys(),
	}
	var spool string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Carry out an upgrade the appliance's interface asked for",
		Long: `Carry out an upgrade the appliance's interface asked for.

Run as root by kubehenge-update.service when a request appears in the spool. It installs only a
release named in the index signed by Kubehenge's release key, and only one newer than itself.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u.Spool = appliance.Spool(spool)
			u.HTTP = &http.Client{Timeout: updateTimeout}
			return u.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&spool, "spool", "/var/lib/kubehenge-update", "the updater's spool directory")
	cmd.Flags().StringVar(&u.ApplianceDir, "appliance-dir", "/var/lib/kubehenge", "the interface's state directory. No upgrade starts while a management cluster run holds its lock")
	cmd.Flags().StringVar(&u.Index, "index", releaseIndexDefault(), "URL of the signed release index (env KUBEHENGE_RELEASE_INDEX)")
	return cmd
}

func newReleaseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Make release keys, indexes and pins, for Kubehenge's maintainers",
	}
	keys := release.PublicKeys()
	cmd.AddCommand(newKeygenCommand(), newSignCommand(keys), newIndexCommand(keys), newRenewCommand(keys), newPinsCommand())
	return cmd
}

// pinsTimeout bounds pinning, a few hundred small downloads.
const pinsTimeout = 30 * time.Minute

// pinsSources are where pinning reads releases: GitHub's API, its release downloads and its raw
// files. Tests point them at a server of their own.
var pinsSources = struct{ api, github, raw string }{"https://api.github.com", "https://github.com", "https://raw.githubusercontent.com"}

func newPinsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pins <releasepins.txt>",
		Short: "Pin the RKE2 and k3s releases a pins file lacks",
		Long: `Add to a pins file (pkg/provisioner/releasepins.txt) every RKE2 and k3s release it lacks, for each
minor version a new cluster may start on and the one before. A pin is the SHA-256 of the release's
checksum file and of its install script, as they are now. A pin already there never changes. With
GH_TOKEN set, it reads GitHub's API at a higher rate limit.`,
		Args: argsSay(cobra.ExactArgs(1), "give the pins file"),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("the pins: %w", err)
			}
			pins, err := provisioner.ParseReleasePins(string(raw))
			if err != nil {
				return err //nolint:wrapcheck // names the file's line
			}
			client := &http.Client{Timeout: time.Minute}
			pinner := &provisioner.ReleasePinner{
				HTTP: client, API: pinsSources.api, Token: os.Getenv("GH_TOKEN"),
				Checksums: &provisioner.ReleaseChecksums{HTTP: client, GitHub: pinsSources.github, Raw: pinsSources.raw},
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), pinsTimeout)
			defer cancel()
			added, err := pinner.Pin(ctx, pins)
			// What was pinned before an error is kept: a pin never changes, so the next run goes on from it.
			if writeErr := os.WriteFile(args[0], []byte(pins.Format(provisioner.PinsHeader(string(raw)))), signatureMode); writeErr != nil { //nolint:gosec // a public list
				return fmt.Errorf("the pins: %w", writeErr)
			}
			for _, v := range added {
				if _, printErr := fmt.Fprintln(cmd.OutOrStdout(), "pinned", v); printErr != nil {
					return fmt.Errorf("printing: %w", printErr)
				}
			}
			return err //nolint:wrapcheck // names the release and the address
		},
	}
}

func newKeygenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen <private key file>",
		Short: "Make a release key pair and print the public key, as a line for pkg/release/release.pub",
		Args:  argsSay(cobra.ExactArgs(1), "give the file to write the private key to"),
		RunE: func(cmd *cobra.Command, args []string) error {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return fmt.Errorf("making the key pair: %w", err)
			}
			f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateKeyMode)
			if err != nil {
				return fmt.Errorf("the private key file: %w", err)
			}
			if _, err = f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"); err != nil {
				_ = f.Close()
				return fmt.Errorf("the private key file: %w", err)
			}
			if err = f.Close(); err != nil {
				return fmt.Errorf("the private key file: %w", err)
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), base64.StdEncoding.EncodeToString(pub)); err != nil {
				return fmt.Errorf("writing the public key: %w", err)
			}
			return nil
		},
	}
}

// newSignCommand signs with keys whose public halves are trusted, the release keys built into this
// binary. A signature from any other key would be refused by every appliance, and by the next
// release's index build, which checks the previous index against them. Each key signs a line of
// its own: during a rotation the old key and the new one both sign, so that appliances trusting
// either take the index. A build without release keys signs with any key.
func newSignCommand(trusted []ed25519.PublicKey) *cobra.Command {
	return &cobra.Command{
		Use:   "sign <private key file>... <index.json>",
		Short: "Check an index and write its signatures to <index.json>.sig",
		Args:  argsSay(cobra.MinimumNArgs(2), "give one or more private key files, then the index"),
		RunE: func(_ *cobra.Command, args []string) error {
			keyFiles, indexFile := args[:len(args)-1], args[len(args)-1]
			index, err := os.ReadFile(indexFile) //nolint:gosec // the file the person running the command named
			if err != nil {
				return fmt.Errorf("the index: %w", err)
			}
			var sigs strings.Builder
			var signers []ed25519.PublicKey
			for _, f := range keyFiles {
				key, pub, keyErr := readReleaseKey(f, trusted)
				if keyErr != nil {
					return keyErr
				}
				if slices.ContainsFunc(signers, func(k ed25519.PublicKey) bool { return k.Equal(pub) }) {
					return fmt.Errorf("%s: the same key twice", f)
				}
				signers = append(signers, pub)
				sigs.WriteString(release.Sign(index, key) + "\n")
			}
			if _, err = release.Parse(index, sigs.String(), signers, time.Now()); err != nil {
				return err //nolint:wrapcheck // release.Parse names the index
			}
			if err = os.WriteFile(indexFile+".sig", []byte(sigs.String()), signatureMode); err != nil {
				return fmt.Errorf("the signature: %w", err)
			}
			return nil
		},
	}
}

// readReleaseKey reads a private release key and its public half, one of trusted unless that is
// empty.
func readReleaseKey(path string, trusted []ed25519.PublicKey) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the file the person running the command named
	if err != nil {
		return nil, nil, fmt.Errorf("the private key file: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(string(trimNewline(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, nil, fmt.Errorf("%s: the private key file holds no Ed25519 seed", path)
	}
	key := ed25519.NewKeyFromSeed(seed)
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, nil, errors.New("the private key has no Ed25519 public key")
	}
	if len(trusted) > 0 && !slices.ContainsFunc(trusted, func(k ed25519.PublicKey) bool { return k.Equal(pub) }) {
		return nil, nil, fmt.Errorf("%s: the private key is not a release key this build trusts (pkg/release/release.pub)", path)
	}
	return key, pub, nil
}

// newRenewCommand writes the published index again with a new expiry, for signing: the releases
// it lists stay as they are, checked against this build's release keys.
func newRenewCommand(keys []ed25519.PublicKey) *cobra.Command {
	return &cobra.Command{
		Use:   "renew <index.json>",
		Short: "Write a published index again, expiring two weeks from now",
		Long: `Write the published index at <index.json> again to standard output, expiring two weeks from now.
It is checked against this build's release keys, with its signatures beside it as <index.json>.sig,
whatever its own expiry. Sign the result with "kubehenge release sign".`,
		Args: argsSay(cobra.ExactArgs(1), "give the published index.json"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(keys) == 0 {
				return errors.New("this build carries no release key to check the index with")
			}
			ix, err := previousIndex(args[0], keys)
			if err != nil {
				return err
			}
			return writeIndex(cmd, ix)
		},
	}
}

// writeIndex writes ix to standard output, expiring release.Validity from now.
func writeIndex(cmd *cobra.Command, ix *release.Index) error {
	ix.Renew(time.Now())
	out, err := ix.Marshal()
	if err != nil {
		return fmt.Errorf("the index: %w", err)
	}
	if _, err = cmd.OutOrStdout().Write(out); err != nil {
		return fmt.Errorf("writing the index: %w", err)
	}
	return nil
}

func newIndexCommand(keys []ed25519.PublicKey) *cobra.Command {
	var previous string
	var add release.Release
	var tofu release.Artefact
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Write the next release index, adding one release to the verified previous one",
		Long: `Write the next release index to standard output, expiring two weeks from now. It is the previous
published index, checked against this build's release keys, with one newer release added. Sign the
result with "kubehenge release sign".`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ix := &release.Index{Releases: []release.Release{}}
			if previous != "" {
				var err error
				if ix, err = previousIndex(previous, keys); err != nil {
					return err
				}
			}
			if tofu != (release.Artefact{}) {
				add.Tofu = &tofu
			}
			if err := ix.Add(add); err != nil {
				return fmt.Errorf("release %s: %w", add.Version, err)
			}
			return writeIndex(cmd, ix)
		},
	}
	f := cmd.Flags()
	f.StringVar(&previous, "previous", "", "the previous release's index.json, with its signature beside it as index.json.sig. Empty for the first release")
	f.StringVar(&add.Version, "version", "", "the new release's version")
	f.StringVar(&add.URL, "url", "", "https URL of the new release's archive")
	f.StringVar(&add.SHA256, "sha256", "", "SHA-256 of the archive, in hex")
	f.StringVar(&tofu.Version, "tofu-version", "", "the OpenTofu release the new release's appliance runs")
	f.StringVar(&tofu.URL, "tofu-url", "", "https URL of that OpenTofu archive")
	f.StringVar(&tofu.SHA256, "tofu-sha256", "", "SHA-256 of the OpenTofu archive, in hex")
	for _, name := range []string{"version", "url", "sha256"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// previousIndex is the published index at path, checked against keys whatever its expiry, since
// it is being signed again.
func previousIndex(path string, keys []ed25519.PublicKey) (*release.Index, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the file the person running the command named
	if err != nil {
		return nil, fmt.Errorf("the previous index: %w", err)
	}
	sig, err := os.ReadFile(path + ".sig") //nolint:gosec // beside the file the person running the command named
	if err != nil {
		return nil, fmt.Errorf("the previous index's signature: %w", err)
	}
	ix, err := release.Verify(raw, string(sig), keys)
	if err != nil {
		return nil, fmt.Errorf("the previous index: %w", err)
	}
	return ix, nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
