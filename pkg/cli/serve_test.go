package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
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

func TestServeStopsCleanlyWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	o, conn := serveOptionsIn(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.run(ctx, conn); err != nil {
		t.Errorf("run gave %v once its context ended; want a clean stop", err)
	}
	if o.s.Appliance == nil || o.s.Upgrades == nil || o.s.MakeTemplate == nil || o.s.CertificateExpires.IsZero() {
		t.Errorf("the interface is not the appliance's: appliance %v, upgrades %v, certificate expiring %s", o.s.Appliance, o.s.Upgrades, o.s.CertificateExpires)
	}
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
