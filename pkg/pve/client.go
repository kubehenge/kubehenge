// Package pve connects to the Proxmox VE API.
package pve

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// Config locates the Proxmox VE API and how to sign in.
type Config struct {
	// URL is the API's address, such as https://pve.example.org:8006.
	URL string
	// Token is an API token as user@realm!name=secret.
	Token string
	// CAFile is a PEM file with the CA that signed the API's certificate, or any certificate of its
	// chain, each taken as a trust anchor. Empty means the system's trust store.
	CAFile string
	// Insecure skips verifying the API's certificate, as the administrator chose at install.
	Insecure bool
}

// Connect returns a client for the API that signs in with the token. It makes no request.
func Connect(cfg Config) (*proxmox.Client, error) {
	id, secret, ok := strings.Cut(cfg.Token, "=")
	if !ok || !strings.Contains(id, "!") || secret == "" {
		return nil, errors.New("the Proxmox VE API token must be user@realm!name=secret")
	}
	base, httpClient, err := cfg.transport()
	if err != nil {
		return nil, err
	}
	httpClient.Transport = statusChecked{httpClient.Transport}
	return proxmox.NewClient(base, proxmox.WithHTTPClient(httpClient), proxmox.WithAPIToken(id, secret)), nil
}

// ConnectSession returns a client for the API that acts with a user's session ticket, or the API
// token in its place, for work done on behalf of a signed-in user. It makes no request.
func ConnectSession(cfg Config, t *Ticket) (*proxmox.Client, error) {
	if t != nil && t.Token != "" {
		return Connect(Config{URL: cfg.URL, Token: t.Token, CAFile: cfg.CAFile, Insecure: cfg.Insecure})
	}
	if t == nil || t.Ticket == "" || t.CSRF == "" {
		return nil, errors.New("a Proxmox VE session ticket with its CSRF token is needed")
	}
	base, httpClient, err := cfg.transport()
	if err != nil {
		return nil, err
	}
	httpClient.Transport = statusChecked{httpClient.Transport}
	return proxmox.NewClient(base, proxmox.WithHTTPClient(httpClient), proxmox.WithSession(t.Ticket, t.CSRF)), nil
}

// Upstream returns the API's address and a transport that verifies its certificate, for passing on
// calls that carry credentials of their own. cfg.Token is not used. It makes no request.
func (cfg Config) Upstream() (*url.URL, http.RoundTripper, error) {
	_, httpClient, err := cfg.transport()
	if err != nil {
		return nil, nil, err
	}
	u, err := url.Parse(strings.TrimSuffix(cfg.URL, "/"))
	if err != nil {
		return nil, nil, fmt.Errorf("the Proxmox VE URL %q: %w", cfg.URL, err)
	}
	return u, httpClient.Transport, nil
}

// statusChecked turns into an error every answer go-proxmox would take as success without
// looking. go-proxmox fails only on 400, 500 and 501 and maps 401 and 403 to ErrNotAuthorized, so a
// 502 to 504 from a proxy in front of the API, or a 595 or 596 when pveproxy cannot reach the node
// a call is forwarded to, would otherwise read as a write that happened.
type statusChecked struct{ next http.RoundTripper }

func (s statusChecked) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(req)
	if err != nil {
		return nil, err //nolint:wrapcheck // a RoundTripper passes its transport's error on; http.Client adds the method and URL
	}
	switch c := resp.StatusCode; {
	case c >= 200 && c < 300, c == http.StatusBadRequest, c == http.StatusUnauthorized, c == http.StatusForbidden,
		c == http.StatusInternalServerError, c == http.StatusNotImplemented:
		return resp, nil
	}
	_ = resp.Body.Close()
	return nil, &APIError{Method: req.Method, Path: req.URL.Path, Status: resp.Status}
}

// transport returns the API's base URL and an HTTP client that verifies its certificate.
func (cfg Config) transport() (string, *http.Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", nil, fmt.Errorf("the Proxmox VE URL %q must be https://host[:port]", cfg.URL)
	}
	var pem []byte
	if cfg.CAFile != "" {
		if pem, err = os.ReadFile(cfg.CAFile); err != nil {
			return "", nil, fmt.Errorf("the Proxmox VE CA: %w", err)
		}
	}
	t, err := sharedTransport(pem, cfg.Insecure)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", cfg.CAFile, err)
	}
	return strings.TrimSuffix(u.String(), "/") + "/api2/json", &http.Client{Timeout: requestTimeout, Transport: t}, nil
}

const (
	requestTimeout = 5 * time.Minute
	// idleConnsPerHost keeps the connections in use reusable: the interface asks about up to eight
	// clusters at once and the controller reconciles four, while Go's default of two idle
	// connections per host would close most of them after each request.
	idleConnsPerHost = 16
	// idleConnTimeout drops a connection before pveproxy closes it after five idle seconds.
	idleConnTimeout = 3 * time.Second
)

// connections sends reads over kept connections and every other request over a connection of its
// own. pveproxy answers some calls, query-url-metadata among them, inside the worker that took
// them. While one waits, that worker serves nothing, and afterwards it drops every connection it
// held, requests already sent on them included. Go sends a dropped read again on a new connection,
// which only a worker that is serving accepts, but never a write.
type connections struct{ reads, writes *http.Transport }

func (c connections) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		return c.reads.RoundTrip(req) //nolint:wrapcheck // a RoundTripper passes its transport's error on; http.Client adds the method and URL
	}
	return c.writes.RoundTrip(req) //nolint:wrapcheck // as above
}

// transports holds one connection pool per CA, keyed by its PEM ("" for the system's trust store)
// and whether verification is skipped, shared by every client: each new connection costs pveproxy
// a TLS handshake, and the credentials go with each request, never with the connection.
var transports = struct {
	sync.Mutex

	byPEM map[transportKey]connections
}{byPEM: map[transportKey]connections{}}

type transportKey struct {
	pem      string
	insecure bool
}

func sharedTransport(pem []byte, insecure bool) (connections, error) {
	transports.Lock()
	defer transports.Unlock()
	key := transportKey{pem: string(pem), insecure: insecure}
	if t, ok := transports.byPEM[key]; ok {
		return t, nil
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Only on the administrator's --proxmox-insecure.
		InsecureSkipVerify: insecure, //nolint:gosec // the administrator's explicit choice for this API
	}
	if len(pem) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return connections{}, errors.New("no PEM certificate in it")
		}
		tlsConfig.RootCAs = pool
	}
	defaults, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return connections{}, errors.New("http.DefaultTransport has been replaced by one that cannot be cloned")
	}
	reads := defaults.Clone()
	reads.TLSClientConfig = tlsConfig
	reads.Proxy = http.ProxyFromEnvironment
	reads.MaxIdleConnsPerHost = idleConnsPerHost
	reads.IdleConnTimeout = idleConnTimeout
	writes := reads.Clone()
	writes.DisableKeepAlives = true
	c := connections{reads: reads, writes: writes}
	transports.byPEM[key] = c
	return c, nil
}
