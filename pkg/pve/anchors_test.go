package pve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type issued struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

// issue signs the template with the parent's key, or with its own where parent is nil.
func issue(t *testing.T, template *x509.Certificate, parent *issued) *issued {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template.SerialNumber = serial
	template.NotBefore, template.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	signer, signerKey := template, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &issued{cert: cert, der: der, key: key}
}

func authority(name string) *x509.Certificate {
	return &x509.Certificate{
		Subject: pkix.Name{Organization: []string{"Example"}, CommonName: name},
		IsCA:    true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
}

// The installer may pin the root of the chain the API serves, an intermediate or the API's own
// certificate. The appliance, OpenTofu's and Cluster API's Proxmox VE providers, the cloud
// controller manager and the CSI plugin are all Go, whose verification takes every certificate it
// is given as a trust anchor, as this client does.
func TestEveryCertificateOfTheChainIsATrustAnchor(t *testing.T) {
	t.Parallel()
	root := issue(t, authority("Example Org Root CA"), nil)
	issuing := issue(t, authority("Example Org Issuing CA"), root)
	other := issue(t, authority("Example Other Root CA"), nil)
	leaf := issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "pve1.example.org"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, issuing)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.der, issuing.der}, PrivateKey: leaf.key}},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	file := func(name string, certs ...*issued) string {
		t.Helper()
		data := make([]byte, 0, len(certs)*1024)
		for _, c := range certs {
			data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})...)
		}
		path := filepath.Join(dir, name+".pem")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	otherFile := file("other", other)
	for _, tc := range []struct {
		name     string
		caFile   string
		insecure bool
		trusted  bool
	}{
		{name: "the root", caFile: file("root", root), trusted: true},
		{name: "the issuing CA, not self-signed", caFile: file("issuing", issuing), trusted: true},
		{name: "the API's own certificate", caFile: file("leaf", leaf), trusted: true},
		{name: "a bundle holding the root", caFile: file("bundle", other, root), trusted: true},
		{name: "another CA", caFile: otherFile},
		{name: "another CA, with verification skipped", caFile: otherFile, insecure: true, trusted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := Connect(Config{URL: srv.URL, Token: "kubehenge@pve!controller=s3cret", CAFile: tc.caFile, Insecure: tc.insecure}) //nolint:gosec // test data
			if err != nil {
				t.Fatal(err)
			}
			var v map[string]any
			err = c.Get(t.Context(), "/version", &v)
			if tc.trusted && err != nil {
				t.Errorf("not trusted: %v", err)
			}
			if !tc.trusted && err == nil {
				t.Error("trusted")
			}
		})
	}
}
