package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/acme"
)

const acmePath = certificatePath + "/acme"

// acmeFlags are the flags of the command that sets ACME. Every secret comes from a file, never from
// an argument, which a process list shows.
type acmeFlags struct {
	directory, email                string
	keyID, hmacKeyFile              string
	directoryCAFile                 string
	proof, profile                  string
	dnsProvider, dnsCredentialsFile string
	resolvers                       []string
}

func (o *applianceOptions) certificateACMECommand() *cobra.Command {
	var f acmeFlags
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
			req, err := f.request()
			if err != nil {
				return err
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				return o.changeCertificate(ctx, cmd, s, http.MethodPut, acmePath, req)
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.directory, "directory", acme.DirectoryLetsEncrypt, "ACME directory: letsencrypt, letsencrypt-staging or an https address")
	fl.StringVar(&f.email, "email", "", "contact e-mail address for the ACME account")
	fl.StringVar(&f.keyID, "eab-key-id", "", "key ID of the external account binding")
	fl.StringVar(&f.hmacKeyFile, "eab-hmac-key-file", "", "file holding the external account binding's HMAC key")
	fl.StringVar(&f.directoryCAFile, "directory-ca-file", "", "PEM file with the CA of the directory's own certificate, trusted beside the system roots")
	fl.StringVar(&f.proof, "proof", acme.ProofTLSALPN, "proof: tls-alpn-01, http-01 or dns-01")
	fl.StringVar(&f.profile, "profile", "", "certificate profile the ACME server offers, such as shortlived")
	fl.StringVar(&f.dnsProvider, "dns-provider", "", "with dns-01, the name of the DNS provider (see the providers command)")
	fl.StringVar(&f.dnsCredentialsFile, "dns-credentials-file", "", "with dns-01, file of NAME=value lines with the variables the DNS provider reads")
	fl.StringSliceVar(&f.resolvers, "resolver", nil, "with dns-01, a DNS server to check the record against, as address:port, repeatable")
	o.jsonFlag(cmd)
	cmd.AddCommand(o.certificateACMEOrderCommand(), o.certificateACMEProvidersCommand())
	return cmd
}

// request reads the files the flags name into the request the interface takes.
func (f *acmeFlags) request() (*acme.Request, error) {
	req := &acme.Request{
		Directory: f.directory, Email: f.email, Proof: f.proof, Profile: f.profile, DNSProvider: f.dnsProvider, Resolvers: f.resolvers,
	}
	if f.keyID != "" || f.hmacKeyFile != "" {
		hmac, err := secretFrom(f.hmacKeyFile, "")
		if err != nil {
			return nil, err
		}
		req.ExternalAccountBinding = &acme.Binding{KeyID: f.keyID, HMACKey: hmac}
	}
	if f.directoryCAFile != "" {
		raw, err := os.ReadFile(f.directoryCAFile)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f.directoryCAFile, err)
		}
		req.DirectoryCA = string(raw)
	}
	if f.dnsCredentialsFile != "" {
		raw, err := os.ReadFile(f.dnsCredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f.dnsCredentialsFile, err)
		}
		if req.DNSCredentials, err = parseCredentials(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", f.dnsCredentialsFile, err)
		}
	}
	return req, nil
}

// parseCredentials reads NAME=value lines. Blank lines and lines that start with # are skipped.
func parseCredentials(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("line %d is not NAME=value. Write one variable per line", n)
		}
		out[strings.TrimSpace(name)] = value
	}
	return out, nil
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
