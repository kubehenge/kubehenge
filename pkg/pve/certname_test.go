package pve

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// leafFor is a certificate for the names and addresses, as Proxmox VE's own lists them beside
// loopback and localhost.
func leafFor(dns []string, ips ...string) *x509.Certificate {
	cert := &x509.Certificate{DNSNames: append([]string{"localhost"}, dns...)}
	for _, ip := range append([]string{"127.0.0.1", "::1"}, ips...) {
		cert.IPAddresses = append(cert.IPAddresses, net.ParseIP(ip))
	}
	return cert
}

// Fails on a build whose certificateNamed passes x509.HostnameError on as it is, or lists the
// certificate's names or the fix wrongly.
func TestCertificateNameMismatchNamesTheCertificatesAddressesAndTheFix(t *testing.T) {
	t.Parallel()
	reissue := "`pvecm updatecerts --force`"
	for _, c := range []struct {
		name, host string
		cert       *x509.Certificate
		want       []string
	}{
		{
			"only IPv6, reached over IPv4", "172.21.99.2", leafFor([]string{"pve"}, "fd99:21:99::2"),
			[]string{"valid for fd99:21:99::2, pve, not 172.21.99.2.", "make the node's name resolve to 172.21.99.2 first", reissue},
		},
		{
			"only IPv4, reached over IPv6", "2001:db8::11", leafFor(nil, "192.0.2.11"),
			[]string{"valid for 192.0.2.11, not 2001:db8::11.", "resolve to 2001:db8::11 first", reissue},
		},
		{
			"both, reached at another address", "198.51.100.11", leafFor([]string{"pve1.example.org"}, "192.0.2.11", "2001:db8::11"),
			[]string{"valid for 192.0.2.11, 2001:db8::11, pve1.example.org, not 198.51.100.11.", reissue},
		},
		{
			"only a host name, reached at an address", "192.0.2.11", leafFor([]string{"pve1.example.org"}),
			[]string{"valid for pve1.example.org, not 192.0.2.11.", "Use a Proxmox VE URL with one of those", reissue},
		},
		{
			"only a host name, reached at another name", "pve.example.org", leafFor([]string{"pve1.example.org"}),
			[]string{"valid for pve1.example.org, not pve.example.org.", "or install a certificate for pve.example.org on Proxmox VE."},
		},
		{
			"loopback alone", "192.0.2.11", leafFor(nil),
			[]string{"valid for no name or address but loopback, not 192.0.2.11."},
		},
	} {
		err := certificateNamed(&tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: c.cert, Host: c.host}})
		mismatch, ok := errors.AsType[*CertificateNameError](err)
		if !ok {
			t.Errorf("%s: got %v, want a CertificateNameError", c.name, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(mismatch.Error(), w) {
				t.Errorf("%s: %q does not say %q", c.name, mismatch.Error(), w)
			}
		}
		if net.ParseIP(c.host) == nil && strings.Contains(mismatch.Error(), "pvecm") {
			t.Errorf("%s: a name is never reissued by pvecm: %q", c.name, mismatch.Error())
		}
	}
}

// Fails on a build whose certificateNamed turns every TLS failure into a name mismatch.
func TestOtherVerificationFailuresPassUnchanged(t *testing.T) {
	t.Parallel()
	unknown := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	if got := certificateNamed(unknown); !errors.Is(got, unknown) {
		t.Errorf("an unknown CA became %v", got)
	}
	if certificateNamed(nil) != nil {
		t.Error("no error became one")
	}
}

// Fails on a build whose connections.RoundTrip passes the transport's error on as it is: a client
// reaching the API at an address its certificate lacks then reads Go's bare x509 message.
func TestClientsReachingTheAPIAtAnUnlistedAddressAreToldTheListedOnes(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Proxmox VE's own certificate on a dual-stack node whose name resolved to its IPv6 address.
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pve"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost", "pve"},
		IPAddresses: []net.IP{net.ParseIP("::1"), net.ParseIP("fd99:21:99::2")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"version":"9.0"}}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Connect(Config{URL: srv.URL, Token: "kubehenge@pve!controller=s3cret", CAFile: ca}) //nolint:gosec // test data
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Version(context.Background())
	mismatch, ok := errors.AsType[*CertificateNameError](err)
	if !ok {
		t.Fatalf("got %v, want a CertificateNameError", err)
	}
	if mismatch.Host != "127.0.0.1" || strings.Join(mismatch.Names, ",") != "fd99:21:99::2,pve" {
		t.Errorf("got host %s and names %q", mismatch.Host, mismatch.Names)
	}
}
