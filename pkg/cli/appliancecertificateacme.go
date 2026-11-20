package cli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/acme"
)

const acmePath = certificatePath + "/acme"

func (o *applianceOptions) certificateACMECommand() *cobra.Command {
	var f acme.Files
	cmd := &cobra.Command{
		Use:   "acme",
		Short: "Get the interface's certificate from an ACME server and renew it",
		Long: `Get the interface's certificate from an ACME server, such as Let's Encrypt or an organisation's
own, and renew it before it runs out. The certificate is for the host of the interface's public
address. The proof is tls-alpn-01 on port 443, http-01 on port 80 or dns-01 through a DNS provider.

Saving the setting starts the first order and returns. Show the certificate to follow it. The HMAC
key and the DNS credentials are read from files. A file of DNS credentials holds one NAME=value line
per variable the provider reads, as the provider's documentation names them. Choosing another
certificate (replace, self-sign, reset) removes the setting.`,
		Example: `  kubehenge appliance certificate acme --proof tls-alpn-01
  kubehenge appliance certificate acme --directory https://ca.example.org/acme/directory --directory-ca-file ca.pem --eab-key-id kid --eab-hmac-key-file hmac.txt
  kubehenge appliance certificate acme --proof dns-01 --dns-provider rfc2136 --dns-credentials-file dns.env --resolver 192.0.2.53:53`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := f.Request()
			if err != nil {
				return fmt.Errorf("the ACME options: %w", err)
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodPut, acmePath, req)
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.Directory, "directory", acme.DirectoryLetsEncrypt, "ACME directory: letsencrypt, letsencrypt-staging or an https address")
	fl.StringVar(&f.Email, "email", "", "contact e-mail address for the ACME account")
	fl.StringVar(&f.KeyID, "eab-key-id", "", "key ID of the external account binding")
	fl.StringVar(&f.HMACKeyFile, "eab-hmac-key-file", "", "file holding the external account binding's HMAC key")
	fl.StringVar(&f.DirectoryCAFile, "directory-ca-file", "", "PEM file with the CA of the directory's own certificate, trusted beside the system roots")
	fl.StringVar(&f.Proof, "proof", acme.ProofTLSALPN, "proof: tls-alpn-01, http-01 or dns-01")
	fl.StringVar(&f.Profile, "profile", "", "certificate profile the ACME server offers, such as shortlived")
	fl.StringVar(&f.DNSProvider, "dns-provider", "", "with dns-01, the name of the DNS provider (see the providers command)")
	fl.StringVar(&f.DNSCredentialsFile, "dns-credentials-file", "", "with dns-01, file of NAME=value lines with the variables the DNS provider reads")
	fl.StringSliceVar(&f.Resolvers, "resolver", nil, "with dns-01, a DNS server to check the record against, as address:port, repeatable")
	o.jsonFlag(cmd)
	cmd.AddCommand(o.certificateACMEOrderCommand(), o.certificateACMEProvidersCommand())
	return cmd
}

func (o *applianceOptions) certificateACMEOrderCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "order",
		Short: "Order the certificate now, as a renewal on demand",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodPost, acmePath+"/order", nil)
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

func (o *applianceOptions) certificateACMEProvidersCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List the DNS providers dns-01 can use",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v struct {
					Providers []string `json:"providers"`
				}
				raw, err := s.call(ctx, http.MethodGet, acmePath+"/providers", nil, &v)
				if err != nil {
					return err
				}
				if o.json {
					return printJSON(cmd, raw)
				}
				for _, name := range v.Providers {
					if err = say(cmd, name); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

func printACME(t *tabwriter.Writer, v *acme.View) {
	if v == nil {
		return
	}
	row(t, "ACME", v.State)
	row(t, "ACME directory", v.Directory)
	proof := v.Proof
	if v.DNSProvider != "" {
		proof += " through " + v.DNSProvider
	}
	row(t, "ACME proof", proof)
	if len(v.DNSCredentialsSet) > 0 {
		row(t, "ACME credentials set", strings.Join(v.DNSCredentialsSet, ", "))
	}
	if v.Error != "" {
		when := ""
		if v.ErrorAt != nil {
			when = " (" + v.ErrorAt.UTC().Format(time.RFC3339) + ")"
		}
		row(t, "ACME error", v.Error+when)
	}
	if v.GivenUp {
		row(t, "ACME retries", "stopped after a day of failures. Save the setting or order again to start afresh")
	} else if v.NextAttempt != nil {
		row(t, "ACME retry", v.NextAttempt.UTC().Format(time.RFC3339))
	}
	if v.LastSuccess != nil {
		row(t, "ACME last issued", v.LastSuccess.UTC().Format(time.RFC3339))
	}
	if v.NextRenewal != nil {
		row(t, "ACME next renewal", v.NextRenewal.UTC().Format(time.RFC3339))
	}
}
