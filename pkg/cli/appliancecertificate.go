package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
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
	ApplianceCA        *caAnswer  `json:"applianceCA"`
}

// caAnswer is the appliance's own CA.
type caAnswer struct {
	Fingerprint string   `json:"fingerprint"`
	Permits     []string `json:"permits"`
	PEM         string   `json:"pem"`
}

// certificateCommand shows and changes the certificate the interface serves, as its Administration
// page does.
func (o *applianceOptions) certificateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certificate",
		Short: "Show or change the certificate the interface serves",
		Long: `Show the certificate the interface serves. Its commands change it, with no restart, as the
interface's certificate page does.`,
		Args: noArgs,
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
	cmd.AddCommand(o.certificateReplaceCommand(), o.certificateSelfSignCommand(), o.certificateApplianceCACommand(), o.certificateCACommand(),
		o.certificateResetCommand(), o.certificateACMECommand(), o.certificateAddressCommand())
	return cmd
}

func (o *applianceOptions) certificateAddressCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "address <name>",
		Short: "Change the DNS name or IP address users reach the interface at",
		Long: `Change the DNS name or IP address users reach the interface at, with no restart. A self-signed
certificate or one of the appliance's CA is made again for it, and ACME orders one. Any other
certificate stays until one for the new name replaces it.`,
		Example: `  kubehenge appliance certificate address kubehenge.example.org`,
		Args:    argsSay(cobra.ExactArgs(1), "give the DNS name or IP address"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodPut, certificatePath+"/address", map[string]string{"name": args[0]})
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

func (o *applianceOptions) certificateApplianceCACommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "appliance-ca",
		Short: "Serve a certificate the appliance's own CA signs for the public address",
		Long: `Serve a certificate the appliance's own CA signs for the public address and the names the current
one covers. The CA may sign only for these names, so trusting it trusts nothing else. Print it
with "kubehenge appliance certificate ca".`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodPost, certificatePath+"/appliance-ca", nil)
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

func (o *applianceOptions) certificateCACommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ca",
		Short:   "Print the appliance's own CA as PEM, to trust it",
		Example: `  kubehenge appliance certificate ca > kubehenge-ca.pem`,
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v certificateAnswer
				if _, err := s.call(ctx, http.MethodGet, certificatePath, nil, &v); err != nil {
					return err
				}
				if v.ApplianceCA == nil {
					return errors.New(`the appliance has no CA of its own. Make one with "kubehenge appliance certificate appliance-ca"`)
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), v.ApplianceCA.PEM)
				return err //nolint:wrapcheck // a failed write to standard output says what failed
			})
		},
	}
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
		Short: "Serve a new self-signed certificate for the public address",
		Long: `Serve a new self-signed certificate for the public address and the names the current one covers.
Browsers warn about it until they trust it.`,
		Args: noArgs,
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
		raw, err := readNamedFile(path)
		if err != nil {
			return formBody{}, err
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
	if v.ApplianceCA != nil {
		row(t, "Appliance CA", v.ApplianceCA.Fingerprint+", for "+strings.Join(v.ApplianceCA.Permits, ", "))
	}
	printACME(t, v.ACME)
	return flush(t)
}
