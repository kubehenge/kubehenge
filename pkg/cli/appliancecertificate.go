package cli

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/acme"
)

const certificatePath = "/api/appliance/certificate"

// certificateAnswer is what the interface says of the certificate it serves.
type certificateAnswer struct {
	Source             string     `json:"source"`
	Subject            string     `json:"subject"`
	DNSNames           []string   `json:"dnsNames"`
	IPAddresses        []string   `json:"ipAddresses"`
	Issuer             string     `json:"issuer"`
	NotBefore          time.Time  `json:"notBefore"`
	NotAfter           time.Time  `json:"notAfter"`
	Fingerprint        string     `json:"fingerprint"`
	Chain              []string   `json:"chain"`
	Incomplete         bool       `json:"incomplete"`
	MissingIssuer      string     `json:"missingIssuer"`
	SelfSigned         bool       `json:"selfSigned"`
	PublicURL          string     `json:"publicURL"`
	PublicNameResolves *bool      `json:"publicNameResolves"`
	NotServed          string     `json:"notServed"`
	LeftOut            []string   `json:"leftOut"`
	ACME               *acme.View `json:"acme"`
}

// certificateCommand shows and changes the certificate the interface serves, as its Administration
// page does.
func (o *applianceOptions) certificateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certificate",
		Short: "Show the certificate the interface serves",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v certificateAnswer
				if err := o.get(ctx, cmd, s, certificatePath, &v); err != nil {
					return err
				}
				return printCertificate(cmd, &v)
			})
		},
	}
	o.jsonFlag(cmd)
	cmd.AddCommand(o.certificateReplaceCommand(), o.certificateSelfSignCommand(), o.certificateResetCommand(), o.certificateACMECommand())
	return cmd
}

func (o *applianceOptions) certificateReplaceCommand() *cobra.Command {
	var passphraseFile string
	cmd := &cobra.Command{
		Use:   "replace <file>...",
		Short: "Serve the certificate in the files given, in any format",
		Long: `Serve the certificate in the files given, from the next connection on, with no restart. The files
hold the certificate, its key and any intermediates, as PEM, DER, PKCS#7 or PKCS#12, in one file
or several, in any order. An encrypted key or PKCS#12 file takes its passphrase from
--passphrase-file. The interface's checks apply: the certificate must be valid for the interface's
public name and for at least another day.`,
		Example: `  kubehenge appliance certificate replace fullchain.pem host.key
  kubehenge appliance certificate replace host.pfx --passphrase-file ~/passphrase`,
		Args: argsSay(cobra.MinimumNArgs(1), "give the certificate files"),
		RunE: func(cmd *cobra.Command, paths []string) error {
			pass := ""
			if passphraseFile != "" {
				var err error
				if pass, err = secretFrom(passphraseFile, ""); err != nil {
					return err
				}
			}
			body, err := uploadBody(paths, pass)
			if err != nil {
				return err
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodPut, certificatePath, body)
			})
		},
	}
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", "file holding the passphrase of an encrypted key or PKCS#12 file")
	o.jsonFlag(cmd)
	return cmd
}

func (o *applianceOptions) certificateSelfSignCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self-sign",
		Short: "Serve a new self-signed certificate for the names the current one covers",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodPost, certificatePath+"/self-signed", nil)
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

func (o *applianceOptions) certificateResetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Serve the installer's certificate again",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodDelete, certificatePath, nil)
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

// changeCertificate asks for a change and prints the certificate served after it.
func (o *applianceOptions) changeCertificate(ctx context.Context, cmd *cobra.Command, s *applianceSession, method, path string, body any) error {
	var v certificateAnswer
	raw, err := s.call(ctx, method, path, body, &v)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(cmd, raw)
	}
	return printCertificate(cmd, &v)
}

// uploadBody is the files and the passphrase as the interface takes them.
func uploadBody(paths []string, passphrase string) (formBody, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, path := range paths {
		raw, err := os.ReadFile(path) //nolint:gosec // the file the person running the command named
		if err != nil {
			return formBody{}, fmt.Errorf("reading %s: %w", path, err)
		}
		part, err := w.CreateFormFile("file", filepath.Base(path))
		if err != nil {
			return formBody{}, fmt.Errorf("encoding %s: %w", path, err)
		}
		if _, err = part.Write(raw); err != nil {
			return formBody{}, fmt.Errorf("encoding %s: %w", path, err)
		}
	}
	if passphrase != "" {
		if err := w.WriteField("passphrase", passphrase); err != nil {
			return formBody{}, fmt.Errorf("encoding the passphrase: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return formBody{}, fmt.Errorf("encoding the files: %w", err)
	}
	return formBody{contentType: w.FormDataContentType(), data: buf.Bytes()}, nil
}

func printCertificate(cmd *cobra.Command, v *certificateAnswer) error {
	t := table(cmd)
	row(t, "Source", v.Source)
	row(t, "Subject", v.Subject)
	row(t, "Names", orNone(strings.Join(slices.Concat(v.DNSNames, v.IPAddresses), ", ")))
	row(t, "Issuer", v.Issuer)
	row(t, "Valid", v.NotBefore.UTC().Format(time.DateOnly)+" to "+v.NotAfter.UTC().Format(time.DateOnly))
	row(t, "Fingerprint", v.Fingerprint)
	row(t, "Chain", strings.Join(v.Chain, " > "))
	if v.SelfSigned {
		row(t, "Self-signed", "yes")
	}
	if v.Incomplete {
		row(t, "Incomplete", "the chain lacks "+v.MissingIssuer)
	}
	if v.PublicURL != "" {
		resolves := ""
		switch {
		case v.PublicNameResolves == nil:
		case *v.PublicNameResolves:
			resolves = ", resolves to this appliance"
		default:
			resolves = ", does not resolve to this appliance"
		}
		row(t, "Public URL", v.PublicURL+resolves)
	}
	if v.NotServed != "" {
		row(t, "Not served", "the stored certificate, since "+v.NotServed)
	}
	if len(v.LeftOut) > 0 {
		row(t, "Left out", strings.Join(v.LeftOut, ", "))
	}
	printACME(t, v.ACME)
	return flush(t)
}
