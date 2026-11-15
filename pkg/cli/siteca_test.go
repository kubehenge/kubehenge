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

func authorityFile(t *testing.T, name string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	text := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	path := filepath.Join(t.TempDir(), name+".crt")
	if err = os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, text
}

func TestControllerSiteCAReachesTheReleaseMirrorAndTheNodes(t *testing.T) {
	t.Parallel()
	sitePath, site := authorityFile(t, "site")
	ownPath, own := authorityFile(t, "own")

	o := &controllerOptions{siteCAFile: sitePath}
	o.releaseMirror.URL = "https://mirror.example.org"
	if err := o.parseReleaseMirror(); err != nil {
		t.Fatal(err)
	}
	if o.releaseMirror.CABundle != strings.TrimSpace(site) || o.r.SiteCA != strings.TrimSpace(site) {
		t.Errorf("a mirror without an authority: %q, nodes %q, want the site CA", o.releaseMirror.CABundle, o.r.SiteCA)
	}

	o = &controllerOptions{siteCAFile: sitePath, releaseMirrorCA: ownPath}
	o.releaseMirror.URL = "https://mirror.example.org"
	if err := o.parseReleaseMirror(); err != nil {
		t.Fatal(err)
	}
	if o.releaseMirror.CABundle != own {
		t.Errorf("a mirror with its own authority: %q, want its own", o.releaseMirror.CABundle)
	}

	o = &controllerOptions{}
	o.releaseMirror.URL = "https://mirror.example.org"
	if err := o.parseReleaseMirror(); err != nil || o.releaseMirror.CABundle != "" || o.r.SiteCA != "" {
		t.Errorf("no site CA: %v, %q, %q", err, o.releaseMirror.CABundle, o.r.SiteCA)
	}

	o = &controllerOptions{siteCAFile: sitePath}
	o.releaseMirror.URL = "http://mirror.example.org"
	if err := o.parseReleaseMirror(); err != nil || o.releaseMirror.CABundle != "" {
		t.Errorf("a plain HTTP mirror: %v, %q", err, o.releaseMirror.CABundle)
	}
}

func TestControllerRefusesABadSiteCAAndTimeZone(t *testing.T) {
	t.Parallel()
	bad := filepath.Join(t.TempDir(), "bad.crt")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &controllerOptions{siteCAFile: bad}
	if err := o.parseReleaseMirror(); err == nil || !strings.Contains(err.Error(), "--site-ca") || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Errorf("a file without a certificate gave %v", err)
	}
	o = &controllerOptions{siteCAFile: filepath.Join(t.TempDir(), "missing")}
	if err := o.parseReleaseMirror(); err == nil || !strings.Contains(err.Error(), "--site-ca") {
		t.Errorf("a missing file gave %v", err)
	}
	o = &controllerOptions{timeZone: "Mars/Olympus"}
	if err := o.parseReleaseMirror(); err == nil || !strings.Contains(err.Error(), "--time-zone") {
		t.Errorf("a time zone that is none gave %v", err)
	}
}
