package acme

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/go-acme/lego/v5/challenge/http01"
	"github.com/go-acme/lego/v5/challenge/tlsalpn01"
)

// ALPNProtocol is the protocol an ACME server offers to validate the tls-alpn-01 proof.
const ALPNProtocol = tlsalpn01.ACMETLS1Protocol

// challenges are the answers pending for the proofs the interface itself serves.
type challenges struct {
	mu    sync.RWMutex
	certs map[string]*tls.Certificate
	paths map[string]string
}

type alpnProvider struct{ c *challenges }

func (p *alpnProvider) Present(_ context.Context, domain, _, keyAuth string) error {
	cert, err := tlsalpn01.ChallengeCert(domain, keyAuth)
	if err != nil {
		return err //nolint:wrapcheck // lego names the failure
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if p.c.certs == nil {
		p.c.certs = map[string]*tls.Certificate{}
	}
	p.c.certs[strings.ToLower(domain)] = cert
	return nil
}

func (p *alpnProvider) CleanUp(_ context.Context, domain, _, _ string) error {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	delete(p.c.certs, strings.ToLower(domain))
	return nil
}

type httpProvider struct{ c *challenges }

func (p *httpProvider) Present(_ context.Context, _, token, keyAuth string) error {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if p.c.paths == nil {
		p.c.paths = map[string]string{}
	}
	p.c.paths[http01.ChallengePath(token)] = keyAuth
	return nil
}

func (p *httpProvider) CleanUp(_ context.Context, _, token, _ string) error {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	delete(p.c.paths, http01.ChallengePath(token))
	return nil
}

// ChallengeCertificate answers a ClientHello that offers acme-tls/1 for a name with a challenge
// pending. It gives nil for any other handshake.
func (m *Manager) ChallengeCertificate(hello *tls.ClientHelloInfo) *tls.Certificate {
	if hello == nil || !slices.Contains(hello.SupportedProtos, ALPNProtocol) {
		return nil
	}
	m.challenges.mu.RLock()
	defer m.challenges.mu.RUnlock()
	return m.challenges.certs[strings.ToLower(hello.ServerName)]
}

// ALPNPending tells whether a tls-alpn-01 challenge waits for a validation, which is when the
// listener offers acme-tls/1.
func (m *Manager) ALPNPending(hello *tls.ClientHelloInfo) bool {
	return m.ChallengeCertificate(hello) != nil
}

// ConfigForClient gives the listener that serves base the protocols a handshake may negotiate. Only
// while a tls-alpn-01 challenge is pending for the name asked for, and only when the client offers
// it, does it add acme-tls/1, so that every other handshake negotiates what it would without ACME.
func (m *Manager) ConfigForClient(base *tls.Config) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if !m.ALPNPending(hello) {
			return nil, nil // nil keeps the listener's own configuration
		}
		cfg := base.Clone()
		cfg.NextProtos = append(slices.Clone(cfg.NextProtos), ALPNProtocol)
		return cfg, nil
	}
}

// HTTPHandler answers the http-01 challenges that are pending and sends every other request to the
// same path on https, keeping the host.
func (m *Manager) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.challenges.mu.RLock()
		keyAuth, ok := m.challenges.paths[r.URL.Path]
		m.challenges.mu.RUnlock()
		if ok && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(keyAuth))
			return
		}
		target := url.URL{Scheme: "https", Host: hostOf(r.Host), Path: r.URL.Path, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
	})
}

// hostOf is the host of a Host header without its port, with the brackets an IPv6 address needs.
func hostOf(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i >= 0 && !strings.HasSuffix(hostport, "]") {
		hostport = hostport[:i]
	}
	return hostport
}
