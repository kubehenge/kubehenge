package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A leaf the intermediate signs is valid to OpenSSL with the root alone as its trust, and Go's
// verifier refuses it: the split a lane with this authority shows.
func TestOpenSSLAcceptsGoRefuses(t *testing.T) {
	t.Parallel()
	a, err := newAuthority(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.root.RawSubject, a.intermediate.RawIssuer) || a.root.Subject.String() != a.intermediate.Issuer.String() {
		t.Fatalf("the intermediate's issuer %q is not the root's name %q in other bytes", a.intermediate.Issuer, a.root.Subject)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "registry.example"},
		DNSNames:    []string{"registry.example"},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leaf, err := sign(template, a.intermediate, &leafKey.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}

	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(a.root)
	intermediates.AddCert(a.intermediate)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: "registry.example"})
	if !errors.As(err, new(x509.UnknownAuthorityError)) {
		t.Errorf("Go's verifier: %v, want an unknown authority", err)
	}
	// The copy for a trust store carries the chain for Go, as a root the host's own store holds.
	store := x509.NewCertPool()
	store.AddCert(a.storeRoot)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: store, Intermediates: intermediates, DNSName: "registry.example"}); err != nil {
		t.Errorf("Go's verifier with the store's copy: %v", err)
	}
	if !bytes.Equal(a.storeRoot.RawSubject, a.intermediate.RawIssuer) || !bytes.Equal(a.storeRoot.RawSubjectPublicKeyInfo, a.root.RawSubjectPublicKeyInfo) {
		t.Error("the store's copy is not the root's key under the intermediate's issuer bytes")
	}

	dir := t.TempDir()
	for name, cert := range map[string]*x509.Certificate{"root.pem": a.root, "intermediate.pem": a.intermediate, "leaf.pem": leaf} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	verify := exec.CommandContext(t.Context(), "openssl", "verify", "-CAfile", "root.pem", "-untrusted", "intermediate.pem", "leaf.pem")
	verify.Dir = dir
	if out, err := verify.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("leaf.pem: OK")) {
		t.Errorf("openssl verify: %v: %s", err, out)
	}
}

// mitmproxy reads its key first and signs with the first certificate. The root after it is what
// makes it send the intermediate.
func TestWrite(t *testing.T) {
	t.Parallel()
	a, err := newAuthority(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "conf")
	if err = a.write(dir); err != nil {
		t.Fatal(err)
	}
	store, err := os.ReadFile(filepath.Join(dir, "mitmproxy-ca.pem")) //nolint:gosec // the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	var blocks []*pem.Block
	for block, rest := pem.Decode(store); block != nil; block, rest = pem.Decode(rest) {
		blocks = append(blocks, block)
	}
	if len(blocks) != 3 || blocks[0].Type != "PRIVATE KEY" ||
		!bytes.Equal(blocks[1].Bytes, a.intermediate.Raw) || !bytes.Equal(blocks[2].Bytes, a.root.Raw) {
		t.Errorf("mitmproxy-ca.pem holds %d blocks, not the key, the intermediate and the root", len(blocks))
	}
	key, err := x509.ParsePKCS8PrivateKey(blocks[0].Bytes)
	if err != nil || !a.key.Equal(key) {
		t.Errorf("mitmproxy-ca.pem's key is not the intermediate's: %v", err)
	}
	root, err := os.ReadFile(filepath.Join(dir, "root.pem")) //nolint:gosec // the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	if block, rest := pem.Decode(root); block == nil || !bytes.Equal(block.Bytes, a.root.Raw) || len(bytes.TrimSpace(rest)) > 0 {
		t.Error("root.pem is not the root alone")
	}
	stored, err := os.ReadFile(filepath.Join(dir, "store-root.pem")) //nolint:gosec // the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	if block, rest := pem.Decode(stored); block == nil || !bytes.Equal(block.Bytes, a.storeRoot.Raw) || len(bytes.TrimSpace(rest)) > 0 {
		t.Error("store-root.pem is not the store's copy alone")
	}
}
