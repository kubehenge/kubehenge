package appliance

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
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

// issuedFiles is a certificate for 127.0.0.1 that is its own CA, as the files the interface keeps.
type issuedFiles struct {
	cert, key []byte
	pair      tls.Certificate
}

func issueFor127(t *testing.T, serial int64) issuedFiles {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return issuedFiles{
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		pair: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
	}
}

func writeFile(t *testing.T, name string, data ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	var all []byte
	for _, d := range data {
		all = append(all, d...)
	}
	if err := os.WriteFile(path, all, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// After an administrator gave the interface a certificate, an upgrade's health check expects that
// one, since the installer's file is no longer what is served. A stored file that does not read is
// not served, so the check goes back to the installer's.
func TestServiceCheckExpectsTheCertificateAnAdministratorGave(t *testing.T) {
	t.Parallel()
	installer, given := issueFor127(t, 1), issueFor127(t, 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{given.pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	check := func(stored string) error {
		c := &ServiceCheck{
			CertFile: writeFile(t, "tls.crt", installer.cert), StoredFile: stored, Address: srv.Listener.Addr().String(),
			Wait: time.Second, Settle: time.Millisecond, Restarts: func(context.Context) (int, error) { return 0, nil },
		}
		return c.Healthy(t.Context())
	}
	if err := check(writeFile(t, InterfaceCertificateFile, []byte("# source: uploaded\n"), given.key, given.cert)); err != nil {
		t.Errorf("serving the stored certificate: %v", err)
	}
	for name, stored := range map[string]string{
		"no stored file": filepath.Join(t.TempDir(), "none"), "a stored file that does not read": writeFile(t, InterfaceCertificateFile, []byte("garbage")),
	} {
		if err := check(stored); err == nil || !strings.Contains(err.Error(), "serves a certificate other than") || !strings.Contains(err.Error(), "tls.crt") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStoredInterfaceCertificateReadsOnlyAFileTheInterfaceWouldServe(t *testing.T) {
	t.Parallel()
	given := issueFor127(t, 3)
	leaf, err := StoredInterfaceCertificate(writeFile(t, InterfaceCertificateFile, []byte("# source: self-signed\n"), given.key, given.cert), time.Now())
	if err != nil || leaf.SerialNumber.Int64() != 3 {
		t.Errorf("a good file: %v %v", leaf, err)
	}
	for name, data := range map[string][]byte{"no key": given.cert, "no certificate": given.key, "nothing": nil} {
		if _, err := StoredInterfaceCertificate(writeFile(t, InterfaceCertificateFile, data), time.Now()); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if _, err := StoredInterfaceCertificate(writeFile(t, InterfaceCertificateFile, given.key, given.cert), time.Now().AddDate(1, 0, 0)); err == nil {
		t.Error("a certificate that ran out was read")
	}
}
