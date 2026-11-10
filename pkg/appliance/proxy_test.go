package appliance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

//nolint:gosec // fake credentials, which the tests look for in every output
const (
	proxyAddress      = "http://proxy.example.com:3128"
	proxyWithPassword = "http://alice:s3cret-pw@proxy.example.com:3128"
)

// passwordProxy is a proxy that asks for a user and password.
func passwordProxy() *Proxy {
	return &Proxy{URL: proxyAddress, User: "alice", Password: "s3cret-pw"}
}

// certificatePEM is a self-signed certificate, which is all a bundle needs to parse.
func certificatePEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "proxy CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// tfvars is the part of the module's variables file the proxy tests read.
type tfvars struct {
	Settings tfvarsSettings `json:"settings"`
}

type tfvarsSettings struct {
	Proxy map[string]string `json:"proxy"`
}

func checkProxy(p *Proxy) error {
	var pr problems
	checkProxySetting(p, &pr)
	return pr.err()
}

func TestProxyValidation(t *testing.T) {
	t.Parallel()
	ca := certificatePEM(t)
	good := []*Proxy{
		nil,
		{},
		{URL: "http://proxy.example.com:3128"},
		{URL: "https://proxy.example.com"},
		{URL: proxyAddress + "/", User: "alice", Password: "s3cret-pw"},
		{URL: proxyAddress, User: "alice"},
		{URL: "http://[2001:db8::1]:3128", NoProxy: []string{".example.com", "10.0.0.0/8", "pve.example.org:8006", "*.corp"}},
		{URL: "http://proxy.example.com", CABundle: ca},
	}
	for _, p := range good {
		if err := checkProxy(p); err != nil {
			t.Errorf("%+v refused: %v", p, err)
		}
	}
	bad := map[string]*Proxy{
		"no scheme":           {URL: "proxy.example.com:3128"},
		"socks":               {URL: "socks5://proxy.example.com:1080"},
		"no host":             {URL: "http://:3128"},
		"a path":              {URL: "http://proxy.example.com/pac"},
		"a query":             {URL: "http://proxy.example.com/?a=b"},
		"credentials inside":  {URL: proxyWithPassword},
		"a user inside":       {URL: "http://alice@proxy.example.com"},
		"a password only":     {URL: proxyAddress, Password: "pw"},
		"exceptions alone":    {NoProxy: []string{"a.example.com"}},
		"authority alone":     {CABundle: ca},
		"bad exception":       {URL: "http://proxy.example.com", NoProxy: []string{"a b"}},
		"comma in exception":  {URL: "http://proxy.example.com", NoProxy: []string{"a,b"}},
		"bad network":         {URL: "http://proxy.example.com", NoProxy: []string{"10.0.0.0/99"}},
		"no certificate":      {URL: "http://proxy.example.com", CABundle: "not pem"},
		"a private key":       {URL: "http://proxy.example.com", CABundle: ca + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"},
		"too many exceptions": {URL: "http://proxy.example.com", NoProxy: slices.Repeat([]string{"a.example.com"}, maxNoProxy+1)},
	}
	for name, p := range bad {
		if err := checkProxy(p); err == nil {
			t.Errorf("%s: %+v accepted", name, p)
		}
	}
}

func TestProxyMessagesNeverCarryTheAddress(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"alice:s3cret-pw@proxy.example.com", "ftp://alice:s3cret-pw@proxy.example.com", "http://alice:s3cret-pw@proxy.example.com/path?q=1", "http://alice:s3cret-pw@",
		"http://alice:s3cret-pw@proxy.example.com:99999999", "http://alice:s3cret-pw@%zz",
	} {
		err := checkProxy(&Proxy{URL: raw, NoProxy: []string{"bad entry"}})
		if err == nil {
			t.Fatalf("%q accepted", raw)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "alice") {
			t.Errorf("%q: error %q carries the credentials", raw, err)
		}
	}
}

func TestRedactedHidesThePasswordEverywhereItIsShown(t *testing.T) {
	t.Parallel()
	s := &Settings{Proxy: passwordProxy()}
	s.Proxy.NoProxy = []string{"a.example.com"}
	shown := s.Redacted()
	raw, err := json.Marshal(shown)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Errorf("the interface's copy carries the password: %s", raw)
	}
	for _, want := range []string{"alice", "proxy.example.com:3128", `"passwordSet":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the interface's copy lacks %q: %s", want, raw)
		}
	}
	if s.Proxy.Password != "s3cret-pw" {
		t.Error("redacting changed the settings themselves")
	}
	if (*Settings)(nil).Redacted() != nil {
		t.Error("no settings redact to some")
	}
}

func TestKeepsProxySecret(t *testing.T) {
	t.Parallel()
	saved := &Settings{Proxy: passwordProxy()}
	// What the interface got is what it sends back unless the administrator changes it.
	back := &Settings{Proxy: saved.Redacted().Proxy}
	back.KeepsProxySecret(saved)
	if back.Proxy.Password != "s3cret-pw" || back.Proxy.PasswordSet {
		t.Errorf("proxy = %+v, want the saved password and no flag", back.Proxy)
	}
	typed := &Settings{Proxy: &Proxy{URL: proxyAddress, User: "alice", Password: "new"}}
	typed.KeepsProxySecret(saved)
	if typed.Proxy.Password != "new" {
		t.Errorf("a typed password was replaced by %q", typed.Proxy.Password)
	}
	cleared := &Settings{Proxy: &Proxy{URL: proxyAddress, User: "alice", ClearPassword: true}}
	cleared.KeepsProxySecret(saved)
	if cleared.Proxy.Password != "" || cleared.Proxy.ClearPassword {
		t.Errorf("a cleared password came back: %+v", cleared.Proxy)
	}
	other := &Settings{Proxy: &Proxy{URL: proxyAddress, User: "bob"}}
	other.KeepsProxySecret(saved)
	if other.Proxy.Password != "" {
		t.Error("another user got alice's password")
	}
	(&Settings{}).KeepsProxySecret(saved)
	(&Settings{Proxy: &Proxy{URL: "http://x"}}).KeepsProxySecret(nil)
}

func TestNoProxyListHoldsWhatTheClustersNeed(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.Proxy = &Proxy{URL: "http://proxy.example.com:3128", NoProxy: []string{".corp.example", "localhost", "10.244.0.0/16"}}
	s.TemplateRefresh = &TemplateRefresh{Enabled: true, Address: "192.0.2.50/24", Gateway: "192.0.2.1"}
	got := s.NoProxyList("https://pve.example.org:8006")
	ranges, err := s.ranges()
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(
		[]string{"localhost", "127.0.0.1", ".svc", ".cluster.local", "pve.example.org"},
		[]string{"10.0.0.0/24", "192.0.2.0/24"}, ranges.pods, ranges.services, []string{".corp.example"},
	)
	if !slices.Equal(got, want) {
		t.Errorf("list = %v\nwant   %v", got, want)
	}
	if (&Settings{}).NoProxyList("https://pve.example.org") != nil {
		t.Error("without a proxy there is nothing to bypass")
	}
}

func TestEnvironmentHoldsTheProxyInBothSpellings(t *testing.T) {
	t.Parallel()
	p := passwordProxy()
	got := p.Env([]string{"a.example.com", "10.0.0.0/8"})
	for _, want := range []string{
		"HTTP_PROXY=" + proxyWithPassword, "HTTPS_PROXY=" + proxyWithPassword, "NO_PROXY=a.example.com,10.0.0.0/8",
		"http_proxy=" + proxyWithPassword, "https_proxy=" + proxyWithPassword, "no_proxy=a.example.com,10.0.0.0/8",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("%q missing from %v", want, got)
		}
	}
	if (&Proxy{}).Env(nil) != nil || (*Proxy)(nil).Env(nil) != nil {
		t.Error("no proxy, no environment")
	}
}

func TestModuleGetsTheProxy(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	if got := moduleProxy(s, "https://pve.example.org:8006"); got["url"] != "" || got["no_proxy"] != "" || got["ca_bundle"] != "" {
		t.Errorf("without a proxy the module gets %v", got)
	}
	ca := certificatePEM(t)
	s.Proxy = passwordProxy()
	s.Proxy.NoProxy, s.Proxy.CABundle = []string{".corp.example"}, ca
	got := moduleProxy(s, "https://pve.example.org:8006")
	if got["url"] != proxyWithPassword || got["ca_bundle"] != ca {
		t.Errorf("module proxy = %v", got)
	}
	noProxy, ok := got["no_proxy"].(string)
	if !ok {
		t.Fatalf("no_proxy = %v, want a string", got["no_proxy"])
	}
	for _, want := range []string{"pve.example.org", "localhost", ".svc", ".cluster.local", "10.0.0.0/24", ".corp.example"} {
		if !slices.Contains(strings.Split(noProxy, ","), want) {
			t.Errorf("no_proxy %q lacks %q", noProxy, want)
		}
	}
}

func TestOpenTofuGetsTheSettingsProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://from-the-service:1")
	a := &Appliance{Dir: t.TempDir(), ProxmoxURL: "https://pve.example.org:8006"}
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		t.Fatal(err)
	}
	s := settings("pve1")
	ca := certificatePEM(t)
	s.Proxy = passwordProxy()
	s.Proxy.CABundle = ca
	if err := a.writeConfig(s, VMIDRange{DefaultVMIDStart, DefaultVMIDEnd}); err != nil {
		t.Fatal(err)
	}
	env, err := a.env("a passphrase of 16+ chars", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "HTTPS_PROXY="+proxyWithPassword) || slices.Contains(env, "HTTPS_PROXY=http://from-the-service:1") {
		t.Errorf("the settings' proxy must replace the service's: %v", env)
	}
	file := filepath.Join(a.workdir(), "proxy-ca.pem")
	if !slices.Contains(env, "SSL_CERT_FILE="+file) {
		t.Error("SSL_CERT_FILE missing from the environment")
	}
	if written, readErr := os.ReadFile(file); readErr != nil || string(written) != ca { //nolint:gosec // the test's own file
		t.Errorf("the authority file holds %q, %v", written, readErr)
	}
	vars, err := os.ReadFile(filepath.Join(a.workdir(), "terraform.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded tfvars
	if err := json.Unmarshal(vars, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Settings.Proxy["url"] != proxyWithPassword || decoded.Settings.Proxy["ca_bundle"] != ca {
		t.Errorf("tfvars proxy = %v", decoded.Settings.Proxy)
	}
}

func TestOpenTofuKeepsTheServicesProxyWithoutASetting(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://from-the-service:1")
	a := &Appliance{Dir: t.TempDir()}
	env, err := a.env("a passphrase of 16+ chars", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "HTTPS_PROXY=http://from-the-service:1") {
		t.Errorf("env = %v", env)
	}
}

func TestJobLogNeverCarriesTheProxyPassword(t *testing.T) {
	t.Parallel()
	p := &Proxy{URL: proxyAddress, User: "alice", Password: "p@ss,w0rd"}
	j := &Job{}
	j.Hide(p.Secrets()...)
	for _, line := range []string{
		`error: Get "https://github.com": proxyconnect tcp: http://alice:p%40ss,w0rd@proxy.example.com:3128 refused`,
		"HTTPS_PROXY=http://alice:p%40ss,w0rd@proxy.example.com:3128",
		"password was p%40ss,w0rd or p@ss,w0rd",
	} {
		j.Add(line)
	}
	for _, line := range j.Log() {
		if strings.Contains(line, "w0rd") {
			t.Errorf("the log carries the password: %q", line)
		}
	}
	if view := j.view(); strings.Contains(strings.Join(view.Log, "\n"), "w0rd") {
		t.Errorf("the view carries the password: %v", view.Log)
	}
	j.Add("a line without secrets")
	if got := j.Log(); got[len(got)-1] != "a line without secrets" {
		t.Errorf("hiding changed an innocent line: %q", got[len(got)-1])
	}
}

func TestSavedTransportFollowsTheSettings(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied = append(proxied, r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(proxy.Close)
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(direct.Close)

	dir := t.TempDir()
	a := &Appliance{Dir: dir}
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		t.Fatal(err)
	}
	s := settings("pve1")
	s.Proxy = &Proxy{URL: proxy.URL, NoProxy: []string{"bypassed.example.net"}}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.workdir(), "settings.json"), raw, privateFile); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &SavedTransport{Dir: dir}, Timeout: 10 * time.Second}
	get := func(url string) int {
		t.Helper()
		resp, getErr := client.Get(url) //nolint:noctx // a test server
		if getErr != nil {
			t.Fatal(getErr)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := get("http://downloads.example.net/file"); got != http.StatusNoContent {
		t.Errorf("a download answered %d: it did not go through the proxy", got)
	}
	if got := get(direct.URL); got != http.StatusOK {
		t.Errorf("the loopback server answered %d: the proxy stood in the way", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(proxied, []string{"downloads.example.net"}) {
		t.Errorf("the proxy saw %v, want only the download", proxied)
	}
}

func TestProxyTransportTrustsTheAuthority(t *testing.T) {
	t.Parallel()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	// The refused handshake of the client that distrusts the certificate is the point of the test.
	server.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	server.StartTLS()
	t.Cleanup(server.Close)
	bundle := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	for name, p := range map[string]*Proxy{"without the authority": {URL: "http://127.0.0.1:9"}, "with it": {URL: "http://127.0.0.1:9", CABundle: bundle}} {
		transport, err := p.Transport(nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Get(server.URL) //nolint:noctx // a test server
		if name == "with it" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			_ = resp.Body.Close()
		} else if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: an unknown authority was trusted", name)
		}
	}
	if _, err := (&Proxy{URL: "http://127.0.0.1:9", CABundle: "junk"}).Transport(nil); err == nil {
		t.Error("a bundle without a certificate was accepted")
	}
}
