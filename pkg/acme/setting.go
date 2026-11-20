// Package acme gets the interface's certificate from an ACME server and renews it. The protocol is
// lego's. This package holds the setting, the loop that renews and the answers to the proofs the
// server asks for.
package acme

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-acme/lego/v5/lego"
)

const minSecretLength = 6

// The proofs an ACME server can ask for.
const (
	ProofTLSALPN = "tls-alpn-01"
	ProofHTTP    = "http-01"
	ProofDNS     = "dns-01"
)

// The directories with a name.
const (
	DirectoryLetsEncrypt        = "letsencrypt"
	DirectoryLetsEncryptStaging = "letsencrypt-staging"
)

// Setting is what the interface orders its certificate with. It holds secrets, so it lives in a
// file only the interface reads.
type Setting struct {
	// Generation tells one saved setting from the next. An order started under one is not delivered
	// under another.
	Generation string `json:"generation"`
	// Directory is the ACME directory's address.
	Directory string `json:"directory"`
	Email     string `json:"email,omitempty"`
	// KeyID and HMACKey are the external account binding some servers want. HMACKey is a secret.
	KeyID   string `json:"keyID,omitempty"`
	HMACKey string `json:"hmacKey,omitempty"`
	// DirectoryCA is a PEM CA for the directory's own TLS, trusted beside the system's roots.
	DirectoryCA string `json:"directoryCA,omitempty"`
	Proof       string `json:"proof"`
	// Profile is the certificate profile the server offers, such as shortlived.
	Profile string `json:"profile,omitempty"`
	// DNSProvider names a lego DNS provider. DNSCredentials are the environment variables that
	// provider reads, and are secrets.
	DNSProvider    string            `json:"dnsProvider,omitempty"`
	DNSCredentials map[string]string `json:"dnsCredentials,omitempty"`
	// Resolvers are the DNS servers to check a DNS record's propagation against.
	Resolvers []string `json:"resolvers,omitempty"`
}

// Request is what an administrator asks for, as the API and the CLI send it. A secret left empty
// keeps the one already saved when the setting names the same key ID or DNS provider, and a
// directory CA left empty keeps the one saved for the same directory.
type Request struct {
	// Directory is letsencrypt, letsencrypt-staging or an https address.
	Directory              string   `json:"directory"`
	Email                  string   `json:"email,omitempty"`
	ExternalAccountBinding *Binding `json:"externalAccountBinding,omitempty"`
	DirectoryCA            string   `json:"directoryCA,omitempty"`
	Proof                  string   `json:"proof"`
	Profile                string   `json:"profile,omitempty"`
	DNSProvider            string   `json:"dnsProvider,omitempty"`
	// DNSCredentials are the variables the provider reads, by name. A name with an empty value
	// keeps the value saved under it.
	DNSCredentials map[string]string `json:"dnsCredentials,omitempty"`
	Resolvers      []string          `json:"resolvers,omitempty"`
}

// Binding is an external account binding.
type Binding struct {
	KeyID string `json:"keyID"`
	// HMACKey is the key the server gave, base64url encoded.
	HMACKey string `json:"hmacKey,omitempty"`
}

// RefusalError is a request the interface refuses, with a code the page acts on and a sentence it
// shows.
type RefusalError struct {
	Code    string
	Message string
}

func (r *RefusalError) Error() string { return r.Message }

func refuse(code, message string, args ...any) *RefusalError {
	return &RefusalError{Code: code, Message: fmt.Sprintf(message, args...)}
}

// The codes of a refusal.
const (
	CodeNoPublicName  = "acme-no-public-name"
	CodeDirectory     = "acme-directory"
	CodeProof         = "acme-proof"
	CodeBinding       = "acme-binding"
	CodeDirectoryCA   = "acme-directory-ca"
	CodeEmail         = "acme-email"
	CodeResolvers     = "acme-resolvers"
	CodeDNSProvider   = "acme-dns-provider"
	CodeDNSCredential = "acme-dns-credentials" //nolint:gosec // a refusal's code, not a credential
	CodeNoHTTP        = "acme-no-http-listener"
	CodeNotSet        = "acme-not-set"
	CodeOrdering      = "acme-ordering"
)

// directoryURL turns a named directory into its address and checks an address.
func directoryURL(name string) (string, *RefusalError) {
	switch name {
	case DirectoryLetsEncrypt:
		return lego.DirectoryURLLetsEncrypt, nil
	case DirectoryLetsEncryptStaging:
		return lego.DirectoryURLLetsEncryptStaging, nil
	}
	u, err := url.Parse(name)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return "", refuse(CodeDirectory, "The ACME directory is %s, %s or an https address. Give one of these", DirectoryLetsEncrypt, DirectoryLetsEncryptStaging)
	}
	return u.String(), nil
}

// directoryPool is the system's roots with the directory's CA added, or nil when it has none.
func directoryPool(roots *x509.CertPool, ca string) (*x509.CertPool, error) {
	if roots == nil {
		var err error
		if roots, err = x509.SystemCertPool(); err != nil {
			roots = x509.NewCertPool()
		}
	}
	if ca == "" {
		return roots, nil
	}
	pool := roots.Clone()
	if !pool.AppendCertsFromPEM([]byte(ca)) {
		return nil, errors.New("no certificate in the directory's CA")
	}
	return pool, nil
}

func validPEM(s string) bool {
	rest := []byte(s)
	found := false
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return found
		}
		if block.Type != "CERTIFICATE" {
			return false
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return false
		}
		found = true
	}
}

func validResolver(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	_, perr := netip.ParseAddrPort(net.JoinHostPort(host, port))
	return perr == nil
}

// build turns a request into a setting, taking a secret the request leaves empty from old.
func build(req *Request, old *Setting) (*Setting, *RefusalError) {
	dir, bad := directoryURL(strings.TrimSpace(req.Directory))
	if bad != nil {
		return nil, bad
	}
	set := &Setting{Directory: dir, Email: strings.TrimSpace(req.Email), Proof: req.Proof, Profile: strings.TrimSpace(req.Profile)}
	if set.Email != "" && !validEmail(set.Email) {
		return nil, refuse(CodeEmail, "The contact e-mail address is not an address. Give one such as admin@example.org, or none")
	}
	if !slices.Contains([]string{ProofTLSALPN, ProofHTTP, ProofDNS}, req.Proof) {
		return nil, refuse(CodeProof, "The proof is %s, %s or %s. Give one of these", ProofTLSALPN, ProofHTTP, ProofDNS)
	}
	if bad = bindingOf(req, old, set); bad != nil {
		return nil, bad
	}
	switch {
	case req.DirectoryCA != "":
		if !validPEM(req.DirectoryCA) {
			return nil, refuse(CodeDirectoryCA, "The directory's CA is not PEM certificates. Give the CA certificate in PEM")
		}
		set.DirectoryCA = req.DirectoryCA
	case old != nil && old.Directory == dir:
		// The view names no CA, so a save of the same directory keeps the one saved.
		set.DirectoryCA = old.DirectoryCA
	}
	if bad = dnsOf(req, old, set); bad != nil {
		return nil, bad
	}
	return set, nil
}

func validEmail(s string) bool {
	return !strings.ContainsAny(s, " \t,<>") && strings.Count(s, "@") == 1 && !strings.HasPrefix(s, "@") && !strings.HasSuffix(s, "@")
}

// dnsOf copies the resolvers, the DNS provider and its credentials, which only the dns-01 proof
// takes, keeping a credential the request leaves empty from old for the same provider.
func dnsOf(req *Request, old, set *Setting) *RefusalError {
	for _, r := range req.Resolvers {
		if !validResolver(r) {
			return refuse(CodeResolvers, "The resolver %s is not an address with a port. Give one such as 192.0.2.53:53", r)
		}
	}
	set.Resolvers = slices.Clone(req.Resolvers)
	if req.Proof != ProofDNS {
		if req.DNSProvider != "" || len(req.DNSCredentials) > 0 || len(req.Resolvers) > 0 {
			return refuse(CodeDNSProvider, "A DNS provider, its credentials and resolvers belong to the %s proof only. Remove them or choose %s", ProofDNS, ProofDNS)
		}
		return nil
	}
	if req.DNSProvider == "" {
		return refuse(CodeDNSProvider, "The %s proof needs a DNS provider. Give the name of one", ProofDNS)
	}
	set.DNSProvider = req.DNSProvider
	set.DNSCredentials = map[string]string{}
	if req.DNSCredentials == nil && old != nil && old.DNSProvider == req.DNSProvider {
		set.DNSCredentials = maps.Clone(old.DNSCredentials)
	}
	for name, value := range req.DNSCredentials {
		if !credentialName.MatchString(name) {
			return refuse(CodeDNSCredential, "%q is not the name of a variable. Give names such as CLOUDFLARE_DNS_API_TOKEN", name)
		}
		if value == "" && old != nil && old.DNSProvider == req.DNSProvider {
			value = old.DNSCredentials[name]
		}
		set.DNSCredentials[name] = value
	}
	return nil
}

// bindingOf copies the external account binding, which comes whole or not at all.
func bindingOf(req *Request, old, set *Setting) *RefusalError {
	b := req.ExternalAccountBinding
	if b == nil || (b.KeyID == "" && b.HMACKey == "") {
		return nil
	}
	set.KeyID, set.HMACKey = b.KeyID, b.HMACKey
	if set.HMACKey == "" && old != nil && old.KeyID == set.KeyID {
		set.HMACKey = old.HMACKey
	}
	if set.KeyID == "" || set.HMACKey == "" {
		return refuse(CodeBinding, "An external account binding needs a key ID and an HMAC key. Give both, or neither")
	}
	if _, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(set.HMACKey, "=")); err != nil {
		return refuse(CodeBinding, "The HMAC key is not base64url. Give the key as the ACME server showed it")
	}
	return nil
}

// secrets are the values that must appear in no answer, log line or error.
func (s *Setting) secrets() []string {
	var out []string
	if s.HMACKey != "" {
		out = append(out, s.HMACKey)
	}
	for _, v := range s.DNSCredentials {
		// A value of a few characters (a TTL, a port) is no secret, and removing it would garble the text.
		if len(v) >= minSecretLength {
			out = append(out, v)
		}
	}
	return out
}

// scrub takes every secret out of text.
func (s *Setting) scrub(text string) string {
	if s == nil {
		return text
	}
	for _, secret := range s.secrets() {
		text = strings.ReplaceAll(text, secret, "[removed]")
	}
	return text
}

func (s *Setting) marshal() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encoding the setting: %w", err)
	}
	return raw, nil
}

// State is how far the order has got, kept so that a restart carries on.
type State struct {
	// Want is the id of the order the setting waits for: set by a save or an order on demand,
	// cleared once a certificate carrying it is served.
	Want string `json:"want,omitempty"`
	// Delivered tells that a certificate from this setting was served once. A setting whose
	// certificate is then no longer an ACME one was replaced by an administrator.
	Delivered bool `json:"delivered,omitempty"`
	// Error is the ACME server's own problem text, or ours, without secrets.
	Error        string    `json:"error,omitempty"`
	ErrorAt      time.Time `json:"errorAt,omitzero"`
	FailingSince time.Time `json:"failingSince,omitzero"`
	Attempts     int       `json:"attempts,omitempty"`
	NextAttempt  time.Time `json:"nextAttempt,omitzero"`
	// GivenUp is set after a day of failures, and stays until the setting is saved again or an order
	// is asked for.
	GivenUp     bool      `json:"givenUp,omitempty"`
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	// RenewalFor is the fingerprint of the certificate NextRenewal is for. A restart that finds
	// another certificate computes the time again.
	RenewalFor  string    `json:"renewalFor,omitempty"`
	NextRenewal time.Time `json:"nextRenewal,omitzero"`
	// ARIChecked is when the server's renewal information was last asked.
	ARIChecked time.Time `json:"ariChecked,omitzero"`
}

// Issued is a certificate the server gave.
type Issued struct {
	Generation string
	// OrderID is written into the stored certificate, so that a restart knows it was served.
	OrderID string
	CertPEM []byte
	KeyPEM  []byte
}
