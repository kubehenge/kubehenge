package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// DefaultIndex is where the appliance finds published releases.
const DefaultIndex = "https://github.com/kubehenge/kubehenge/releases/latest/download/index.json"

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

// releaseIndexDefault lets a mirror serve the index; whatever the source, only an index signed by
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
		Root: "/", Restart: appliance.RestartService, Healthy: check.Healthy, Current: release.Version, Key: release.PublicKey(),
	}
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
	cmd.AddCommand(newKeygenCommand(), newSignCommand(release.PublicKey()), newIndexCommand())
	return cmd
}

func newKeygenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen <private key file>",
		Short: "Make a release key pair; prints the public key for pkg/release/release.pub",
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

// newSignCommand signs with a key whose public half is trusted, the release key built into this
// binary; a signature from any other key would be refused by every appliance, and by the next
// release's index build, which checks the previous index against it. A build without a release key
// (nil) signs with any key.
func newSignCommand(trusted ed25519.PublicKey) *cobra.Command {
	return &cobra.Command{
		Use:   "sign <private key file> <index.json>",
		Short: "Check an index and write its signature to <index.json>.sig",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("the private key file: %w", err)
			}
			seed, err := base64.StdEncoding.DecodeString(string(trimNewline(raw)))
			if err != nil || len(seed) != ed25519.SeedSize {
				return errors.New("the private key file holds no Ed25519 seed")
			}
			key := ed25519.NewKeyFromSeed(seed)
			pub, ok := key.Public().(ed25519.PublicKey)
			if !ok {
				return errors.New("the private key has no Ed25519 public key")
			}
			if trusted != nil && !pub.Equal(trusted) {
				return errors.New("the private key is not the release key this build trusts (pkg/release/release.pub)")
			}
			index, err := os.ReadFile(args[1])
			if err != nil {
				return fmt.Errorf("the index: %w", err)
			}
			sig := release.Sign(index, key)
			if _, err = release.Parse(index, sig, pub); err != nil {
				return fmt.Errorf("the index: %w", err)
			}
			if err = os.WriteFile(args[1]+".sig", []byte(sig+"\n"), signatureMode); err != nil { //nolint:gosec // a signature is public
				return fmt.Errorf("the signature: %w", err)
			}
			return nil
		},
	}
}

func newIndexCommand() *cobra.Command {
	var previous string
	var add release.Release
	var tofu release.Artefact
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Write the next release index: the previous one, verified, with one release added",
		Long: `Write the next release index to standard output: the previous published index, checked
against this build's release key, with one newer release added. The result still has to be signed
with "kubehenge release sign".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ix, err := previousIndex(previous)
			if err != nil {
				return err
			}
			if tofu != (release.Artefact{}) {
				add.Tofu = &tofu
			}
			if err = ix.Add(add); err != nil {
				return fmt.Errorf("release %s: %w", add.Version, err)
			}
			out, err := ix.Marshal()
			if err != nil {
				return fmt.Errorf("the index: %w", err)
			}
			if _, err = cmd.OutOrStdout().Write(out); err != nil {
				return fmt.Errorf("writing the index: %w", err)
			}
			return nil
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

// previousIndex is the published index at path, checked against this build's release key, or an
// empty one for the first release.
func previousIndex(path string) (*release.Index, error) {
	if path == "" {
		return &release.Index{Releases: []release.Release{}}, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the file the person running the command named
	if err != nil {
		return nil, fmt.Errorf("the previous index: %w", err)
	}
	sig, err := os.ReadFile(path + ".sig") //nolint:gosec // beside the file the person running the command named
	if err != nil {
		return nil, fmt.Errorf("the previous index's signature: %w", err)
	}
	ix, err := release.Parse(raw, string(sig), release.PublicKey())
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
