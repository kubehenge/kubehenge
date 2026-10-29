package appliance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"
)

func testCertificate(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "admin"}, NotBefore: notAfter.AddDate(-1, 0, 0), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// The first of the admin kubeconfig's and talosconfig's certificates to run out counts.
func TestCredentialsExpire(t *testing.T) {
	t.Parallel()
	kubeAt := time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)
	talosAt := time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC)
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: m, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: admin, user: {client-certificate-data: %s, client-key-data: a2V5}}]
contexts: [{name: m, context: {cluster: m, user: admin}}]
current-context: m
`, testCertificate(t, kubeAt))
	talos := fmt.Sprintf("context: m\ncontexts:\n  m:\n    endpoints: [192.0.2.20]\n    ca: \"\"\n    crt: %s\n    key: a2V5\n", testCertificate(t, talosAt))
	if got := CredentialsExpire([]byte(kubeconfig), []byte(talos)); !got.Equal(talosAt) {
		t.Errorf("expire %v, want %v", got, talosAt)
	}
	if got := CredentialsExpire([]byte(kubeconfig), nil); !got.Equal(kubeAt) {
		t.Errorf("expire %v without a talosconfig, want %v", got, kubeAt)
	}
	if got := CredentialsExpire([]byte("admin-kubeconfig"), nil); !got.IsZero() {
		t.Errorf("expire %v of no certificate", got)
	}
}
