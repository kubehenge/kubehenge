package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
	"github.com/kubehenge/kubehenge/pkg/ui"
)

// writeCertificate writes a certificate for 127.0.0.1 and localhost that is its own CA, and its
// key, into dir, and returns their files.
func writeCertificate(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kubehenge test"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for path, block := range map[string]*pem.Block{certFile: {Type: "CERTIFICATE", Bytes: der}, keyFile: {Type: "PRIVATE KEY", Bytes: keyDER}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certFile, keyFile
}

func TestInterfaceSchemeHoldsClustersAndTheCoreKinds(t *testing.T) {
	t.Parallel()
	scheme, err := interfaceScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{&clusterv1.Cluster{}, &clusterv1.ClusterClass{}, &clusterv1.MachineDeployment{}, &corev1.Secret{}, &corev1.ConfigMap{}} {
		if kinds, _, err := scheme.ObjectKinds(o); err != nil || len(kinds) == 0 {
			t.Errorf("the interface's scheme does not know %T: %v", o, err)
		}
	}
}

// serveOptionsIn is the serve command's flags for an appliance in dir, its certificate there too,
// with a Proxmox VE whose CA is that certificate.
func serveOptionsIn(t *testing.T, dir string) (*serveOptions, *pve.Config) {
	t.Helper()
	certFile, keyFile := writeCertificate(t, dir)
	o := &serveOptions{certFile: certFile, keyFile: keyFile, listen: "127.0.0.1:0", updateSpool: filepath.Join(dir, "spool")}
	o.s.WorkloadPool = "kubehenge"
	o.a.Dir = dir
	o.logs.level = "info"
	return o, &pve.Config{URL: "https://127.0.0.1:8006", CAFile: certFile}
}

func TestServeRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		change func(*serveOptions, *pve.Config)
		want   string
	}{
		{"no certificate", func(o *serveOptions, _ *pve.Config) { o.certFile = "" }, "--tls-cert and --tls-key are required"},
		{"a pool too deep", func(o *serveOptions, _ *pve.Config) { o.s.WorkloadPool = "a/b/c/d/e/f/g/h" }, "--workload-pool"},
		{"a key that is no key", func(o *serveOptions, _ *pve.Config) { o.keyFile = o.certFile }, "--tls-cert and --tls-key:"},
		{"Proxmox VE over HTTP", func(_ *serveOptions, conn *pve.Config) { conn.URL = "http://127.0.0.1:8006" }, "the Proxmox VE API"},
		{"an appliance without the CA", func(_ *serveOptions, conn *pve.Config) { conn.CAFile = "" }, "--proxmox-ca is required in the appliance"},
		{"Headlamp without its upstream", func(o *serveOptions, _ *pve.Config) { o.headlampListen = "127.0.0.1:0" }, "--headlamp-listen needs"},
		{"port 80 without the appliance directory", func(o *serveOptions, _ *pve.Config) { o.a.Dir, o.httpListen = "", ":80" }, "--http-listen needs --appliance-dir"},
		{"a trusted proxy that is a name", func(o *serveOptions, _ *pve.Config) { o.trustedProxies = []string{"proxy.example"} }, `--trusted-proxy: "proxy.example"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, conn := serveOptionsIn(t, t.TempDir())
			c.change(o, conn)
			if err := o.run(t.Context(), conn); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("run gave %v; want %q", err, c.want)
			}
		})
	}
}

// --trusted-proxy repeats and takes lists, and KUBEHENGE_TRUSTED_PROXIES gives the default.
func TestTrustedProxiesComeFromFlagsOrTheEnvironment(t *testing.T) {
	t.Setenv("KUBEHENGE_TRUSTED_PROXIES", "10.0.0.0/8, 192.0.2.7\n2001:db8::/32")
	flags := func(args ...string) []string {
		cmd := newServeCommand(&pve.Config{})
		if err := cmd.Flags().Parse(args); err != nil {
			t.Fatal(err)
		}
		got, err := cmd.Flags().GetStringSlice("trusted-proxy")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := strings.Join(flags(), "|"); got != "10.0.0.0/8|192.0.2.7|2001:db8::/32" {
		t.Errorf("from the environment: %s", got)
	}
	if got := strings.Join(flags("--trusted-proxy", "198.51.100.0/24", "--trusted-proxy=203.0.113.1,203.0.113.2"), "|"); got != "198.51.100.0/24|203.0.113.1|203.0.113.2" {
		t.Errorf("from the flags: %s", got)
	}
}

// A browser's sign-in lasts eight hours and the CLI's a week, unless the flags say otherwise.
func TestSessionLifetimesComeFromTheFlags(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args     []string
		browser  time.Duration
		cli      time.Duration
		scenario string
	}{
		{nil, 8 * time.Hour, 7 * 24 * time.Hour, "defaults"},
		{[]string{"--session-max-age=1h", "--cli-session-max-age=48h"}, time.Hour, 48 * time.Hour, "flags"},
	} {
		cmd := newServeCommand(&pve.Config{})
		if err := cmd.Flags().Parse(c.args); err != nil {
			t.Fatal(err)
		}
		browser, err := cmd.Flags().GetDuration("session-max-age")
		if err != nil {
			t.Fatal(err)
		}
		cli, err := cmd.Flags().GetDuration("cli-session-max-age")
		if err != nil {
			t.Fatal(err)
		}
		if browser != c.browser || cli != c.cli {
			t.Errorf("%s: browser %v, CLI %v", c.scenario, browser, cli)
		}
	}
}

func TestServeStopsCleanlyWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	o, conn := serveOptionsIn(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.run(ctx, conn); err != nil {
		t.Errorf("run gave %v once its context ended; want a clean stop", err)
	}
	if o.s.Appliance == nil || o.s.Upgrades == nil || o.s.MakeTemplate == nil || o.s.Certificate == nil {
		t.Errorf("the interface is not the appliance's: appliance %v, upgrades %v, certificate %v", o.s.Appliance, o.s.Upgrades, o.s.Certificate)
	}
}

// syncBuffer is a buffer the serving goroutines and the test may use together.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs has o's interface log into the buffer it returns.
func captureLogs(o *serveOptions) *syncBuffer {
	b := &syncBuffer{}
	o.s.Log = slog.New(slog.NewTextHandler(b, nil))
	return b
}

var servedAddress = regexp.MustCompile(`msg=serving address=(\S+)`)

// servedAddresses waits for n "serving" lines and returns the addresses they name, in order.
func servedAddresses(t *testing.T, logs *syncBuffer, n int) []string {
	t.Helper()
	for range 200 {
		if m := servedAddress.FindAllStringSubmatch(logs.String(), -1); len(m) >= n {
			addrs := make([]string, 0, n)
			for _, x := range m[:n] {
				addrs = append(addrs, x[1])
			}
			return addrs
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("serve logged no %d serving lines: %s", n, logs.String())
	return nil
}

// A port the interface cannot have stops Headlamp's too, and the command with the reason, once
// what serve started has stopped: Headlamp's first sync has written its kubeconfig, and nothing
// writes after the process would have ended.
func TestServeStopsWithTheFirstServerThatFails(t *testing.T) {
	t.Parallel()
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	o, _ := serveOptionsIn(t, t.TempDir())
	o.listen = taken.Addr().String()
	o.s.Certificate = ui.NewInterfaceCertificate("", o.certFile, o.keyFile)
	if err = o.s.Certificate.Load(time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	o.headlampListen, o.headlampUpstream, o.headlampKubeconfig = "127.0.0.1:0", "http://127.0.0.1:4466", filepath.Join(t.TempDir(), "headlamp.kubeconfig")
	if err = o.setUpHeadlamp(); err != nil {
		t.Fatal(err)
	}
	if err = o.serve(t.Context()); err == nil || !strings.Contains(err.Error(), "serving:") || !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Errorf("serve gave %v; want the taken address named", err)
	}
	if _, err = os.Stat(o.headlampKubeconfig); err != nil {
		t.Errorf("serve returned before Headlamp's sync finished: %v", err)
	}
}

// An address that is already taken is reported at once, naming it, and nothing claims to serve it.
func TestServeReportsAnAddressItCannotBind(t *testing.T) {
	t.Parallel()
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	o, _ := serveOptionsIn(t, t.TempDir())
	o.a.Dir = ""
	logs := captureLogs(o)
	o.s.Certificate = ui.NewInterfaceCertificate("", o.certFile, o.keyFile)
	if err = o.s.Certificate.Load(time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	o.headlampListen, o.headlampUpstream, o.headlampKubeconfig = taken.Addr().String(), "http://127.0.0.1:4466", filepath.Join(t.TempDir(), "headlamp.kubeconfig")
	if err = o.setUpHeadlamp(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- o.serve(t.Context()) }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve kept running with an address it could not bind")
	}
	if err == nil || !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Errorf("serve gave %v; want the taken address named", err)
	}
	if strings.Contains(logs.String(), "serving") {
		t.Errorf("serve logged that it served: %s", logs.String())
	}
}

// The interface's own listener, bound before the taken address failed, is free again.
func TestServeClosesTheListenersItBoundBeforeOneFailed(t *testing.T) {
	t.Parallel()
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	first := &http.Server{Addr: "127.0.0.1:0"}
	listeners, err := listenAll(t.Context(), []*http.Server{first})
	if err != nil {
		t.Fatal(err)
	}
	addr := listeners[0].Addr().String()
	_ = listeners[0].Close()
	if _, err = listenAll(t.Context(), []*http.Server{{Addr: addr}, {Addr: taken.Addr().String()}}); err == nil {
		t.Fatal("listenAll bound a taken address")
	}
	again, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("the listener bound before the failure stayed open: %v", err)
	}
	_ = again.Close()
}

// The interface and Headlamp take the certificate they present from the one holder, which serve
// does not hand them as files.
func TestServeGivesBothListenersTheCertificateOfOneHolder(t *testing.T) {
	t.Parallel()
	o, _ := serveOptionsIn(t, t.TempDir())
	o.a.Dir = ""
	logs := captureLogs(o)
	o.headlampListen, o.headlampUpstream, o.headlampKubeconfig = "127.0.0.1:0", "http://127.0.0.1:4466", filepath.Join(t.TempDir(), "headlamp.kubeconfig")
	o.s.Certificate = ui.NewInterfaceCertificate("", o.certFile, o.keyFile)
	if err := o.s.Certificate.Load(time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := o.setUpHeadlamp(); err != nil {
		t.Fatal(err)
	}
	// The files stay where they are, since what is presented comes from the holder alone.
	for _, f := range []string{o.certFile, o.keyFile} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.serve(ctx) }()
	want := ""
	for _, addr := range servedAddresses(t, logs, 2) {
		var got string
		for range 100 {
			conn, err := (&tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr) //nolint:gosec // only the presented certificate is read
			if err == nil {
				if secured, ok := conn.(*tls.Conn); ok {
					got = string(secured.ConnectionState().PeerCertificates[0].Raw)
				}
				_ = conn.Close()
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if got == "" || want != "" && got != want {
			t.Errorf("%s presents a certificate other than the interface's, or none", addr)
		}
		want = got
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve gave %v once its context ended", err)
	}
}

// An address set on the certificate page before a restart is the name ACME orders for, and the page
// learns Headlamp's port for the callback it writes.
func TestServeTakesTheAddressSetOnThePage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "public-url"), []byte("https://moved.example.org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, _ := serveOptionsIn(t, dir)
	o.s.PublicURL = "https://kubehenge.example.org"
	o.headlampListen = "127.0.0.1:8443"
	if err := o.loadCertificate(time.Now()); err != nil {
		t.Fatal(err)
	}
	if m := o.s.Certificate.ACME(); m == nil || m.PublicName() != "moved.example.org" {
		t.Errorf("ACME orders for %v", m)
	}
	if o.s.Certificate.HeadlampPort != "8443" {
		t.Errorf("Headlamp's port is %q", o.s.Certificate.HeadlampPort)
	}
}

// In the appliance the interface listens on port 80 beside its own port, for ACME's http-01 proof,
// and sends everything else to https. The manager that orders the certificate knows it listens.
func TestServeListensOnPlainHTTPForACMEAndRedirects(t *testing.T) {
	t.Parallel()
	o, _ := serveOptionsIn(t, t.TempDir())
	logs := captureLogs(o)
	o.httpListen = "127.0.0.1:0"
	o.s.PublicURL = "https://kubehenge.example.org"
	if err := o.loadCertificate(time.Now()); err != nil {
		t.Fatal(err)
	}
	m := o.s.Certificate.ACME()
	if m == nil || !m.HTTPListening || m.PublicName() != "kubehenge.example.org" {
		t.Fatalf("the manager is %+v", m)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.serve(ctx) }()
	httpAddr := servedAddresses(t, logs, 2)[1]
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 5 * time.Second}
	var res *http.Response
	for range 100 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+"/clusters?x=1", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "kubehenge.example.org"
		if res, err = hc.Do(req); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if res == nil {
		t.Fatal("port 80 never answered")
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusPermanentRedirect || res.Header.Get("Location") != "https://kubehenge.example.org/clusters?x=1" {
		t.Errorf("port 80 answered %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve gave %v once its context ended", err)
	}
}

func TestSetUpHeadlamp(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                        string
		listen, upstream, iface     string
		kubeconfig, want            string
		wantPort, wantInterfacePort int
	}{
		{name: "none", wantPort: 0},
		{name: "every flag", listen: ":8444", upstream: "http://127.0.0.1:4466", iface: ":8443", kubeconfig: "/run/headlamp", wantPort: 8444, wantInterfacePort: 8443},
		{name: "the default port", listen: "127.0.0.1:", upstream: "http://127.0.0.1:4466", iface: "[::1]:", kubeconfig: "/run/headlamp", wantPort: defaultHTTPSPort, wantInterfacePort: defaultHTTPSPort},
		{name: "no kubeconfig", listen: ":8444", upstream: "http://127.0.0.1:4466", iface: ":8443", want: "--headlamp-upstream and --headlamp-kubeconfig"},
		{name: "upstream over HTTPS", listen: ":8444", upstream: "https://127.0.0.1:4466", iface: ":8443", kubeconfig: "/run/headlamp", want: "http:// address"},
		{name: "upstream without a host", listen: ":8444", upstream: "http://", iface: ":8443", kubeconfig: "/run/headlamp", want: "http:// address"},
		{name: "a listen address without a port", listen: "8444", upstream: "http://127.0.0.1:4466", iface: ":8443", kubeconfig: "/run/headlamp", want: "--headlamp-listen"},
		{name: "the interface's address without a port", listen: ":8444", upstream: "http://127.0.0.1:4466", iface: "8443", kubeconfig: "/run/headlamp", want: "--listen"},
	} {
		o := &serveOptions{headlampListen: c.listen, headlampUpstream: c.upstream, listen: c.iface, headlampKubeconfig: c.kubeconfig}
		err := o.setUpHeadlamp()
		if c.want != "" {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
			}
			continue
		}
		h := o.s.Headlamp
		switch {
		case err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.listen == "" && h != nil:
			t.Errorf("%s: Headlamp is served at %+v", c.name, h)
		case c.listen != "" && (h == nil || h.Port != c.wantPort || h.InterfacePort != c.wantInterfacePort || h.Kubeconfig != c.kubeconfig || h.Upstream.String() != c.upstream):
			t.Errorf("%s: Headlamp is %+v", c.name, h)
		}
	}
}

func TestTheApplianceSetsUpTheInterface(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, conn := serveOptionsIn(t, dir)
	conn.Insecure = true
	scheme, err := interfaceScheme()
	if err != nil {
		t.Fatal(err)
	}
	var kubeconfigs []string
	o.s.NewKube = func(kubeconfig []byte) (client.Client, error) {
		kubeconfigs = append(kubeconfigs, string(kubeconfig))
		return client.New(testRESTConfig(), client.Options{Scheme: scheme})
	}
	if err = o.setUpAppliance(conn); err != nil {
		t.Fatal(err)
	}
	if o.s.Kube != nil || len(kubeconfigs) != 0 {
		t.Error("the interface reaches a management cluster the appliance has not set up")
	}
	if !o.s.ProxmoxInsecure || !o.a.ProxmoxInsecure || o.a.ProxmoxURL != conn.URL || !strings.Contains(o.a.ProxmoxCA, "BEGIN CERTIFICATE") {
		t.Errorf("the appliance reaches Proxmox VE at %s, insecure %v, with CA %.30q", o.a.ProxmoxURL, o.a.ProxmoxInsecure, o.a.ProxmoxCA)
	}
	if o.s.Upgrades.SettingsPath != filepath.Join(dir, "upgrades.json") || string(o.s.Upgrades.Spool) != filepath.Join(dir, "spool") {
		t.Errorf("upgrades are kept in %s and asked for in %s", o.s.Upgrades.SettingsPath, o.s.Upgrades.Spool)
	}
	_, err = o.s.MakeTemplate(t.Context(), &pve.Ticket{Username: "builder@pve", Ticket: "PVE:builder@pve:0"}, templates.Builder{}, &templates.Template{})
	if err == nil || !strings.Contains(err.Error(), "as builder@pve") {
		t.Errorf("a template was made with a ticket without its CSRF token: %v", err)
	}

	// Once the management cluster is up, the interface reaches it with the kubeconfig the appliance keeps.
	if err = os.WriteFile(o.a.KubeconfigPath(), []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = o.setUpAppliance(conn); err != nil {
		t.Fatal(err)
	}
	if o.s.Kube == nil || len(kubeconfigs) != 1 || kubeconfigs[0] != "apiVersion: v1\nkind: Config\n" {
		t.Errorf("the interface reaches %v with %q", o.s.Kube, kubeconfigs)
	}
	conn.CAFile = filepath.Join(dir, "missing.pem")
	if err = o.setUpAppliance(conn); err == nil || !strings.Contains(err.Error(), "--proxmox-ca:") {
		t.Errorf("an appliance without its CA file was set up: %v", err)
	}
}

func TestNewClientTakesTheConfigAsItIs(t *testing.T) {
	t.Parallel()
	scheme, err := interfaceScheme()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newClient(testRESTConfig(), scheme); err != nil {
		t.Errorf("newClient refused a configuration: %v", err)
	}
	cfg := testRESTConfig()
	cfg.Host = "https://[::1"
	if _, err = newClient(cfg, scheme); err == nil || !strings.Contains(err.Error(), "a client of the management cluster") {
		t.Errorf("newClient took an address that is none: %v", err)
	}
}

// testRESTConfig is a management cluster's API that no test reaches: a client is made without a
// request.
func testRESTConfig() *rest.Config {
	return &rest.Config{Host: "https://127.0.0.1:1"}
}
