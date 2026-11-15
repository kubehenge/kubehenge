package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControllerStartsWithAnExpiredSiteCAAndReportsIt(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "site"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Minute),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "site.crt")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &controllerOptions{siteCAFile: path}
	if err = o.parseReleaseMirror(); err != nil {
		t.Fatalf("an expired site CA stopped the controller: %v", err)
	}
	if !strings.Contains(o.r.SiteCAProblem, "expired") {
		t.Errorf("the problem reported to the clusters is %q, want that the authority expired", o.r.SiteCAProblem)
	}
}
