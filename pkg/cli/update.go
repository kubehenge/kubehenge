package cli

import (
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
	"github.com/kubehenge/kubehenge/pkg/release"
)

// DefaultIndex is where the appliance finds published releases: on the website, which signs the
// latest release's index again every week, so that it has not expired.
const DefaultIndex = "https://kubehenge.dev/releases/index.json"

const (
	// updateTimeout bounds the download of a release, the largest file the updater fetches.
	updateTimeout = 10 * time.Minute
	// privateKeyMode keeps the release key to the one who made it.
	privateKeyMode os.FileMode = 0o600
	// signatureMode lets anyone read a signature, which is public.
	signatureMode os.FileMode = 0o644
)

// releaseIndexDefault lets a mirror serve the index; whatever the source, only an index signed by
// the release key is used.
func releaseIndexDefault() string {
	if v := os.Getenv("KUBEHENGE_RELEASE_INDEX"); v != "" {
		return v
	}
	return DefaultIndex
}

func newUpdateCommand() *cobra.Command {
	u := appliance.Updater{Root: "/", Restart: appliance.RestartService, Current: release.Version, Keys: release.PublicKeys()}
	var spool string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Carry out an upgrade the appliance's interface asked for",
		Long: `Carry out an upgrade the appliance's interface asked for.

Run as root by kubehenge-update.service when a request appears in the spool. It installs only a
release named in the index signed by Kubehenge's release key, and only one newer than itself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u.Spool = appliance.Spool(spool)
			u.HTTP = &http.Client{Timeout: updateTimeout}
			return u.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&spool, "spool", "/var/lib/kubehenge-update", "the updater's spool directory")
	cmd.Flags().StringVar(&u.ApplianceDir, "appliance-dir", "/var/lib/kubehenge", "the interface's state directory; no upgrade starts while a management cluster run holds its lock")
	cmd.Flags().StringVar(&u.Index, "index", releaseIndexDefault(), "URL of the signed release index (env KUBEHENGE_RELEASE_INDEX)")
	return cmd
}

func newReleaseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Sign release indexes, for the people who publish Kubehenge",
	}
	keys := release.PublicKeys()
	cmd.AddCommand(newKeygenCommand(), newSignCommand(keys), newIndexCommand(keys), newRenewCommand(keys))
	return cmd
}

func newKeygenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen <private key file>",
		Short: "Make a release key pair; prints the public key, a line for pkg/release/release.pub",
		Args:  cobra.ExactArgs(1),
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
// binary; a signature from any other key would be refused by every appliance, and by the next
// release's index build, which checks the previous index against them. Each key signs a line of
// its own: during a rotation the old key and the new one both sign, so that appliances trusting
// either take the index. A build without release keys signs with any key.
func newSignCommand(trusted []ed25519.PublicKey) *cobra.Command {
	return &cobra.Command{
		Use:   "sign <private key file>... <index.json>",
		Short: "Check an index and write its signatures to <index.json>.sig",
		Args:  cobra.MinimumNArgs(2), //nolint:mnd // a key and the index
		RunE: func(_ *cobra.Command, args []string) error {
			keyFiles, indexFile := args[:len(args)-1], args[len(args)-1]
			index, err := os.ReadFile(indexFile) //nolint:gosec // the file the person running the command named
			if err != nil {
				return fmt.Errorf("the index: %w", err)
			}
			var sigs strings.Builder
			var signers []ed25519.PublicKey
			for _, f := range keyFiles {
				key, err := readReleaseKey(f, trusted)
				if err != nil {
					return err
				}
				pub, _ := key.Public().(ed25519.PublicKey)
				if slices.ContainsFunc(signers, func(k ed25519.PublicKey) bool { return k.Equal(pub) }) {
					return fmt.Errorf("%s: the same key twice", f)
				}
				signers = append(signers, pub)
				sigs.WriteString(release.Sign(index, key) + "\n")
			}
			if _, err = release.Parse(index, sigs.String(), signers, time.Now()); err != nil {
				return fmt.Errorf("the index: %w", err)
			}
			if err = os.WriteFile(indexFile+".sig", []byte(sigs.String()), signatureMode); err != nil { //nolint:gosec // a signature is public
				return fmt.Errorf("the signature: %w", err)
			}
			return nil
		},
	}
}

// readReleaseKey reads a private release key, one of trusted unless that is empty.
func readReleaseKey(path string, trusted []ed25519.PublicKey) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the file the person running the command named
	if err != nil {
		return nil, fmt.Errorf("the private key file: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(string(trimNewline(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: the private key file holds no Ed25519 seed", path)
	}
	key := ed25519.NewKeyFromSeed(seed)
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the private key has no Ed25519 public key")
	}
	if len(trusted) > 0 && !slices.ContainsFunc(trusted, func(k ed25519.PublicKey) bool { return k.Equal(pub) }) {
		return nil, fmt.Errorf("%s: the private key is not a release key this build trusts (pkg/release/release.pub)", path)
	}
	return key, nil
}

// newRenewCommand writes the published index again with a new expiry, for signing: the releases
// it lists stay as they are, checked against this build's release keys.
func newRenewCommand(keys []ed25519.PublicKey) *cobra.Command {
	return &cobra.Command{
		Use:   "renew <index.json>",
		Short: "Write a published index again, expiring two weeks from now",
		Long: `Write a published index again to standard output, expiring two weeks from now: the index
at <index.json>, checked against this build's release keys with its signatures beside it as
<index.json>.sig, whatever its own expiry. The result still has to be signed with
"kubehenge release sign".`,
		Args: cobra.ExactArgs(1),
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
		Short: "Write the next release index: the previous one, verified, with one release added",
		Long: `Write the next release index to standard output, expiring two weeks from now: the previous
published index, checked against this build's release keys, with one newer release added. The
result still has to be signed with "kubehenge release sign".`,
		Args: cobra.NoArgs,
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
	f.StringVar(&previous, "previous", "", "the previous release's index.json, its signature beside it as index.json.sig; none for the first release")
	f.StringVar(&add.Version, "version", "", "the new release's version")
	f.StringVar(&add.URL, "url", "", "https URL of the new release's archive")
	f.StringVar(&add.SHA256, "sha256", "", "SHA-256 of the archive, in hex")
	f.StringVar(&tofu.Version, "tofu-version", "", "the OpenTofu release the appliance runs it with")
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
