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
	"sync/atomic"
	"testing"
	"time"
)

// healthServer serves the health route as the interface does, on a certificate the check is given;
// status is what it answers.
func healthServer(t *testing.T, status *atomic.Int32) *ServiceCheck {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	cert := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &ServiceCheck{
		CertFile: cert, Address: srv.Listener.Addr().String(), Wait: time.Second, Settle: 10 * time.Millisecond,
		Restarts: func(context.Context) (int, error) { return 0, nil },
	}
}

func TestServiceCheck(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	c := healthServer(t, &status)
	if err := c.Healthy(context.Background()); err != nil {
		t.Fatalf("serving: %v", err)
	}

	status.Store(http.StatusBadGateway)
	if err := c.Healthy(context.Background()); err == nil || !strings.Contains(err.Error(), "did not answer within 1s: the interface answered 502") {
		t.Errorf("answering 502: %v", err)
	}

	// Restarted by systemd after it first answered.
	status.Store(http.StatusNoContent)
	var n atomic.Int32
	c.Restarts = func(context.Context) (int, error) { return int(n.Add(2)), nil }
	if err := c.Healthy(context.Background()); err == nil || !strings.Contains(err.Error(), "restarted 2 times") {
		t.Errorf("restarting: %v", err)
	}
}

// Another certificate on the address is not the interface's.
func TestServiceCheckTrustsOnlyItsCertificate(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	c := healthServer(t, &status)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"example.com"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	other.StartTLS()
	t.Cleanup(other.Close)
	c.Address = other.Listener.Addr().String()
	if err := c.Healthy(context.Background()); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("another server's certificate: %v", err)
	}
}
