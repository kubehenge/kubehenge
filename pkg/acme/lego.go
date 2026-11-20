package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"

	legoacme "github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"
	"github.com/go-acme/lego/v5/certcrypto"
	"github.com/go-acme/lego/v5/certificate"
	"github.com/go-acme/lego/v5/challenge"
	"github.com/go-acme/lego/v5/challenge/dns01"
	"github.com/go-acme/lego/v5/lego"
	"github.com/go-acme/lego/v5/providers/dns"
	"github.com/go-acme/lego/v5/registration"
)

// ProviderFunc turns a DNS provider's name and credentials into a provider.
type ProviderFunc func(name string, credentials map[string]string) (challenge.Provider, error)

// ErrUnknownProvider is what a provider constructor answers for a name it does not know.
var ErrUnknownProvider = errors.New("unknown DNS provider")

// envMu serialises the changes legoProvider makes to the process's environment.
var envMu sync.Mutex

// credentialName is what a variable a lego provider reads looks like.
var credentialName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// refusedProviders are lego providers that run a program or read what the appliance must not give an
// administrator of the interface.
var refusedProviders = map[string]string{"exec": "runs a program on the appliance"}

func providerNames() []string {
	names := append(dns.GetProviderCodes(), providerAliases...)
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, refused := refusedProviders[n]; !refused && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// providerAliases are names lego accepts for a provider but leaves out of its list.
var providerAliases = []string{"acme-dns", "rfc2136", "domainnameshop", "fastdns", "linodev4", "webnames"}

// legoProvider asks lego for a DNS provider by name. Lego providers read their credentials from the
// environment, so each is set for the call that builds the provider and put back after. The
// providers keep what they read, so nothing stays in the environment.
//
//nolint:ireturn // lego builds each provider as its own type, known only by this interface
func legoProvider(name string, credentials map[string]string) (challenge.Provider, error) {
	if !slices.Contains(providerNames(), name) {
		return nil, ErrUnknownProvider
	}
	envMu.Lock()
	defer envMu.Unlock()
	restore, err := setEnvironment(credentials)
	defer restore()
	if err != nil {
		return nil, err
	}
	p, err := dns.NewDNSChallengeProviderByName(name)
	if err != nil {
		return nil, fmt.Errorf("building the provider: %w", err)
	}
	return p, nil
}

// setEnvironment sets each variable and returns what puts the environment back.
func setEnvironment(vars map[string]string) (func(), error) {
	type previous struct {
		name, value string
		had         bool
	}
	var saved []previous
	restore := func() {
		for _, p := range saved {
			if p.had {
				_ = os.Setenv(p.name, p.value)
			} else {
				_ = os.Unsetenv(p.name)
			}
		}
	}
	for name, value := range vars {
		if !credentialName.MatchString(name) {
			return restore, fmt.Errorf("%q is not the name of a variable", name)
		}
		old, had := os.LookupEnv(name)
		saved = append(saved, previous{name, old, had})
		if err := os.Setenv(name, value); err != nil {
			return restore, fmt.Errorf("setting %s: %w", name, err)
		}
	}
	return restore, nil
}

// account is the ACME account's key and where the directory knows it.
type account struct {
	Directory string `json:"directory"`
	// URI is the account's address at the directory, empty until it is registered.
	URI string `json:"uri,omitempty"`
	Key string `json:"key"`

	signer crypto.Signer
	email  string
}

func (a *account) GetEmail() string                           { return a.email }
func (a *account) GetPrivateKey() crypto.Signer               { return a.signer }
func (a *account) GetRegistration() *legoacme.ExtendedAccount { return a.registration() }

func (a *account) registration() *legoacme.ExtendedAccount {
	if a.URI == "" {
		return nil
	}
	return &legoacme.ExtendedAccount{Location: a.URI}
}

func (m *Manager) readAccount() (*account, bool) {
	raw, err := m.store.read(accountFile)
	if err != nil {
		return nil, false
	}
	var a account
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	block, _ := pem.Decode([]byte(a.Key))
	if block == nil {
		return nil, false
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	signer, ok := key.(crypto.Signer)
	if err != nil || !ok {
		return nil, false
	}
	a.signer = signer
	return &a, true
}

// accountFor reads the account kept for the directory, or makes a key for one.
func (m *Manager) accountFor(set *Setting) (*account, error) {
	if a, ok := m.readAccount(); ok && a.Directory == set.Directory {
		a.email = set.Email
		return a, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("making the account key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("writing the account key: %w", err)
	}
	a := &account{Directory: set.Directory, Key: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), signer: key, email: set.Email}
	return a, m.keepAccount(a)
}

func (m *Manager) keepAccount(a *account) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encoding the account: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.write(accountFile, raw)
}

const (
	// requestTimeout bounds one request to the ACME server, stepTimeout each step of it.
	requestTimeout = 2 * time.Minute
	stepTimeout    = 30 * time.Second
	// anySleep is longer than any renewal window, so the server's window always gives a time.
	anySleep = 100 * 365 * 24 * time.Hour
)

func (m *Manager) httpClient(set *Setting) (*http.Client, error) {
	pool, err := directoryPool(m.Roots, set.DirectoryCA)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: stepTimeout, KeepAlive: stepTimeout}).DialContext,
			TLSHandshakeTimeout:   stepTimeout,
			ResponseHeaderTimeout: stepTimeout,
			TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}, nil
}

func (m *Manager) client(set *Setting, a *account) (*lego.Client, error) {
	cfg := lego.NewConfig(a)
	cfg.CADirURL = set.Directory
	cfg.UserAgent = "kubehenge"
	hc, err := m.httpClient(set)
	if err != nil {
		return nil, err
	}
	cfg.HTTPClient = hc
	c, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("the ACME client: %w", err)
	}
	return c, nil
}

// register makes the account known to the directory once, with the external account binding when
// the setting holds one.
func (m *Manager) register(ctx context.Context, c *lego.Client, set *Setting, a *account) error {
	if a.URI != "" {
		return nil
	}
	var acct *legoacme.ExtendedAccount
	var err error
	if set.KeyID != "" {
		acct, err = c.Registration.RegisterWithExternalAccountBinding(ctx, registration.RegisterEABOptions{TermsOfServiceAgreed: true, Kid: set.KeyID, HmacEncoded: set.HMACKey})
	} else {
		acct, err = c.Registration.Register(ctx, registration.RegisterOptions{TermsOfServiceAgreed: true})
	}
	if err != nil {
		return fmt.Errorf("registering the ACME account: %w", err)
	}
	a.URI = acct.Location
	return m.keepAccount(a)
}

// legoObtain asks the directory for a certificate for name, answering the setting's proof.
func (m *Manager) legoObtain(ctx context.Context, set *Setting, name string, replaces *x509.Certificate) ([]byte, []byte, error) {
	a, err := m.accountFor(set)
	if err != nil {
		return nil, nil, err
	}
	c, err := m.client(set, a)
	if err != nil {
		return nil, nil, err
	}
	if err = m.setProof(c, set); err != nil {
		return nil, nil, err
	}
	if err = m.register(ctx, c, set, a); err != nil {
		return nil, nil, err
	}
	req := certificate.ObtainRequest{Domains: []string{name}, Bundle: true, KeyType: certcrypto.EC256, Profile: set.Profile}
	if replaces != nil {
		if id, idErr := api.MakeARICertID(replaces); idErr == nil {
			if _, ariErr := c.Certificate.GetRenewalInfo(ctx, replaces); ariErr == nil {
				req.ReplacesCertID = id
			}
		}
	}
	res, err := c.Certificate.Obtain(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("ordering the certificate: %w", err)
	}
	if res == nil || len(res.Certificate) == 0 || len(res.PrivateKey) == 0 {
		return nil, nil, errors.New("the ACME server gave no certificate")
	}
	return res.Certificate, res.PrivateKey, nil
}

// setProof gives the client the one solver the setting names.
func (m *Manager) setProof(c *lego.Client, set *Setting) error {
	var err error
	switch set.Proof {
	case ProofTLSALPN:
		err = c.Challenge.SetTLSALPN01Provider(&alpnProvider{&m.challenges})
	case ProofHTTP:
		err = c.Challenge.SetHTTP01Provider(&httpProvider{&m.challenges})
	case ProofDNS:
		p, perr := m.newProvider()(set.DNSProvider, set.DNSCredentials)
		if perr != nil {
			return fmt.Errorf("the DNS provider: %w", perr)
		}
		opts := dns01.NewOptions()
		var chOpts []dns01.ChallengeOption
		if len(set.Resolvers) > 0 {
			opts.RecursiveNameservers = set.Resolvers
			chOpts = append(chOpts, dns01.DisableAuthoritativeNssPropagationRequirement())
		}
		dns01.SetDefaultClient(dns01.NewClient(opts))
		err = c.Challenge.SetDNS01Provider(p, chOpts...)
	default:
		return fmt.Errorf("the proof %q is unknown", set.Proof)
	}
	if err != nil {
		return fmt.Errorf("setting the %s proof: %w", set.Proof, err)
	}
	return nil
}

// legoARI asks the server when it wants the certificate renewed.
func (m *Manager) legoARI(ctx context.Context, set *Setting, leaf *x509.Certificate) (time.Time, error) {
	a, err := m.accountFor(set)
	if err != nil {
		return time.Time{}, err
	}
	c, err := m.client(set, a)
	if err != nil {
		return time.Time{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	info, err := c.Certificate.GetRenewalInfo(ctx, leaf)
	if err != nil {
		return time.Time{}, fmt.Errorf("asking for renewal information: %w", err)
	}
	at := info.ShouldRenewAt(m.now(), anySleep)
	if at == nil {
		return time.Time{}, errors.New("the renewal window gave no time")
	}
	return *at, nil
}
