package appliance

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// Limits on what the proxy setting may hold, so that it stays a setting and not a file store.
const (
	kibibyte         = 1024
	maxProxyCABundle = 256 * kibibyte
	maxNoProxy       = 200
)

// The schemes a proxy address and a registry mirror may have.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// alwaysNoProxy never goes through a proxy: the machine itself and the management cluster's
// in-cluster names.
var alwaysNoProxy = []string{"localhost", "127.0.0.1", ".svc", ".cluster.local"}

// noProxyEntry is a host name, domain suffix, address or network, optionally with a port.
var noProxyEntry = regexp.MustCompile(`^[A-Za-z0-9*._:/\[\]-]+$`)

// Proxy is the HTTP(S) proxy every download from the management side goes through, for a site
// without direct internet access.
type Proxy struct {
	// URL is the proxy's address, without credentials.
	URL string `json:"url,omitempty"`
	// User and Password sign in to a proxy that asks. The password is never sent to the interface:
	// PasswordSet says that one is saved.
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	// PasswordSet is set on the copy the interface gets, and ignored on one it sends.
	PasswordSet bool `json:"passwordSet,omitempty"`
	// ClearPassword removes the saved password, where an empty Password keeps it.
	ClearPassword bool `json:"clearPassword,omitempty"`
	// NoProxy lists more hosts, domains, addresses and networks that bypass the proxy. The
	// addresses the appliance and the clusters need are added on top.
	NoProxy []string `json:"noProxy,omitempty"`
	// CABundle is the PEM certificate authority that signs the certificates of a proxy that
	// re-signs TLS.
	CABundle string `json:"caBundle,omitempty"`
}

// Enabled tells whether there is a proxy to go through.
func (p *Proxy) Enabled() bool { return p != nil && p.URL != "" }

// redactedPassword is what stands for a proxy password wherever a log would show it.
const redactedPassword = "xxxxx"

// FullURL is the address with the credentials in it, for the components that take a proxy as one
// URL. It is never shown or logged: see Secrets.
func (p *Proxy) FullURL() string {
	if p == nil || p.User == "" {
		if p == nil {
			return ""
		}
		return p.URL
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return p.URL
	}
	if p.Password == "" {
		u.User = url.User(p.User)
	} else {
		u.User = url.UserPassword(p.User, p.Password)
	}
	return u.String()
}

// RedactURL is a proxy URL as it may be shown or logged: the password hidden. An address that does
// not parse is hidden whole, since a parse failure says nothing about where the password is.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	// Without a scheme, "user:pass@host" parses as an opaque URL whose password Redacted cannot see.
	if err != nil || (u.User == nil && strings.Contains(raw, "@")) {
		return "(unreadable address)"
	}
	return u.Redacted()
}

// Redacted is the setting as the interface may show it back.
func (p *Proxy) Redacted() *Proxy {
	if p == nil {
		return nil
	}
	out := *p
	out.PasswordSet = p.Password != ""
	out.Password = ""
	out.ClearPassword = false
	return &out
}

// KeepsSecret fills in the saved password where the setting comes back without one for the same
// user, since the interface never had the password to send again. ClearPassword drops it.
func (p *Proxy) KeepsSecret(saved *Proxy) {
	if p == nil {
		return
	}
	if p.Password == "" && !p.ClearPassword && saved != nil && p.User != "" && p.User == saved.User {
		p.Password = saved.Password
	}
	p.ClearPassword = false
	p.PasswordSet = false
}

// Secrets are the strings a log must never carry: the password and the credential as a URL
// carries it, both as written and as a URL escapes them.
func (p *Proxy) Secrets() []string {
	if p == nil || p.Password == "" {
		return nil
	}
	userinfo := url.UserPassword(p.User, p.Password).String()
	secrets := []string{
		userinfo, p.User + ":" + p.Password,
		url.QueryEscape(p.Password), url.PathEscape(p.Password), p.Password,
	}
	// The password as a URL's userinfo escapes it.
	if _, escaped, found := strings.Cut(userinfo, ":"); found {
		secrets = append(secrets, escaped)
	}
	return secrets
}

// check adds what is wrong with the proxy, in the words of the setup page's fields. No message
// carries the address, which may hold a password.
func checkProxySetting(p *Proxy, pr *problems) {
	if p == nil {
		return
	}
	if p.URL == "" {
		if len(p.NoProxy) > 0 || p.CABundle != "" {
			pr.addf("Proxy address: needed for its exceptions and certificate authority")
		}
		return
	}
	u, err := url.Parse(p.URL)
	switch {
	case err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Hostname() == "":
		pr.addf("Proxy address: an http:// or https:// URL with a host, such as http://proxy.example.com:3128")
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		pr.addf("Proxy address: no path, query or fragment")
	case u.User != nil:
		pr.addf("Proxy address: no user name or password, which have fields of their own")
	}
	if p.Password != "" && p.User == "" {
		pr.addf("Proxy password: needs a user name")
	}
	checkProxyExceptions(p, pr)
	if p.CABundle != "" {
		switch {
		case len(p.CABundle) > maxProxyCABundle:
			pr.addf("Proxy certificate authority: at most %d KiB", maxProxyCABundle/kibibyte)
		case strings.Contains(p.CABundle, "PRIVATE KEY"):
			pr.addf("Proxy certificate authority: certificates only, no private key")
		case !x509.NewCertPool().AppendCertsFromPEM([]byte(p.CABundle)):
			pr.addf("Proxy certificate authority: holds no PEM certificate")
		}
	}
}

func checkProxyExceptions(p *Proxy, pr *problems) {
	if len(p.NoProxy) > maxNoProxy {
		pr.addf("Proxy exceptions: at most %d", maxNoProxy)
	}
	for _, e := range p.NoProxy {
		if !noProxyEntry.MatchString(e) {
			pr.addf("Proxy exceptions: %q is no host name, domain, address or network", e)
			continue
		}
		if strings.Contains(e, "/") {
			if _, err := netip.ParsePrefix(e); err != nil {
				pr.addf("Proxy exceptions: %q is no network such as 10.0.0.0/8", e)
			}
		}
	}
}

// NoProxyList is everything that bypasses the proxy: the machine and the cluster's own names, the
// Proxmox VE API host, the management nodes' network, the build VM's network, the management
// cluster's pod and service ranges, and then the administrator's own entries. Workload clusters'
// node networks are not known here, so the administrator lists those.
func (s *Settings) NoProxyList(proxmoxURL string) []string {
	if !s.Proxy.Enabled() {
		return nil
	}
	list := slices.Clone(alwaysNoProxy)
	if u, err := url.Parse(proxmoxURL); err == nil && u.Hostname() != "" {
		list = append(list, u.Hostname())
	}
	for _, a := range slices.Concat(s.NodeAddresses, s.NodeAddressesIPv6) {
		if prefix, err := netip.ParsePrefix(a); err == nil {
			list = append(list, prefix.Masked().String())
		}
	}
	if s.TemplateRefresh != nil && s.TemplateRefresh.Address != "" {
		if prefix, err := netip.ParsePrefix(s.TemplateRefresh.Address); err == nil {
			list = append(list, prefix.Masked().String())
		}
	}
	if r, err := s.ranges(); err == nil {
		list = append(list, r.pods...)
		list = append(list, r.services...)
	}
	list = append(list, s.Proxy.NoProxy...)
	seen := map[string]bool{}
	return slices.DeleteFunc(list, func(e string) bool {
		dup := seen[e]
		seen[e] = true
		return dup
	})
}

// Env is the proxy as the standard environment variables, in both spellings, for programs that
// read them. noProxy is the finished list from NoProxyList.
func (p *Proxy) Env(noProxy []string) []string {
	if !p.Enabled() {
		return nil
	}
	joined := strings.Join(noProxy, ",")
	full := p.FullURL()
	return []string{
		"HTTP_PROXY=" + full, "HTTPS_PROXY=" + full, "NO_PROXY=" + joined,
		"http_proxy=" + full, "https_proxy=" + full, "no_proxy=" + joined,
	}
}

// Transport is a copy of the default transport that goes through the proxy except for the hosts
// the list names, and trusts the proxy's certificate authority beside the system's.
func (p *Proxy) Transport(noProxy []string) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport has been replaced by one that cannot be cloned")
	}
	t := base.Clone()
	if !p.Enabled() {
		return t, nil
	}
	cfg := httpproxy.Config{HTTPProxy: p.FullURL(), HTTPSProxy: p.FullURL(), NoProxy: strings.Join(noProxy, ",")}
	proxyFunc := cfg.ProxyFunc()
	t.Proxy = func(r *http.Request) (*url.URL, error) { return proxyFunc(r.URL) }
	if p.CABundle != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(p.CABundle)) {
			return nil, errors.New("the proxy certificate authority holds no PEM certificate")
		}
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	return t, nil
}

// SavedTransport sends requests the way the settings saved in an appliance directory say, read
// again whenever the file changes, so that a setting saved while the interface runs reaches the
// downloads it makes without a restart. Without a proxy it behaves as the default transport.
type SavedTransport struct {
	// Dir is the appliance's state directory.
	Dir string
	// ProxmoxURL is added to the exceptions, since the proxy must never stand between the appliance
	// and Proxmox VE.
	ProxmoxURL string

	mu    sync.Mutex
	stamp time.Time
	size  int64
	rt    http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *SavedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt, err := t.current()
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(r) //nolint:wrapcheck // the transport's own errors
}

func (t *SavedTransport) current() (http.RoundTripper, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	path := filepath.Join((&Appliance{Dir: t.Dir}).workdir(), "settings.json")
	info, statErr := os.Stat(path)
	if t.rt != nil && statErr == nil && info.ModTime().Equal(t.stamp) && info.Size() == t.size {
		return t.rt, nil
	}
	saved := (&Appliance{Dir: t.Dir}).SavedSettings()
	var p *Proxy
	var noProxy []string
	if saved != nil {
		p, noProxy = saved.Proxy, saved.NoProxyList(t.ProxmoxURL)
	}
	transport, err := p.Transport(noProxy)
	if err != nil {
		return nil, fmt.Errorf("the saved proxy setting: %w", err)
	}
	if !p.Enabled() {
		// The environment still applies where the appliance's service sets it.
		transport.Proxy = http.ProxyFromEnvironment
	}
	if saved != nil {
		if err = saved.ReleaseMirror.Mirror().Trust(transport); err != nil {
			return nil, fmt.Errorf("the saved release mirror: %w", err)
		}
	}
	t.rt = transport
	if statErr == nil {
		t.stamp, t.size = info.ModTime(), info.Size()
	}
	return t.rt, nil
}
