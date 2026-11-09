package appliance

import (
	"context"
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

// validAuthority is a certificate valid from an hour ago to a day from now.
func validAuthority(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mirror-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestReleaseMirrorValidation(t *testing.T) {
	t.Parallel()
	ca := validAuthority(t)
	for _, m := range []*ReleaseMirror{nil, {}, {URL: "https://mirror.example.org/github"}, {URL: "http://10.0.0.5:8080"}, {URL: "https://mirror.example.org", CABundle: ca}} {
		s := settings("pve1")
		s.ReleaseMirror = m
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", m, err)
		}
	}
	for name, m := range map[string]*ReleaseMirror{
		"no address":         {URL: "mirror.example.org"},
		"a trailing slash":   {URL: "https://mirror.example.org/"},
		"credentials":        {URL: "https://u:p@mirror.example.org"}, //nolint:gosec // a fake address
		"an authority alone": {CABundle: ca},
		"plain HTTP":         {URL: "http://mirror.example.org", CABundle: ca},
		"an expired one":     {URL: "https://mirror.example.org", CABundle: chartAuthority},
		"no certificate":     {URL: "https://mirror.example.org", CABundle: "hello"},
	} {
		s := settings("pve1")
		s.ReleaseMirror = m
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "Release mirror") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReleaseMirrorReachesTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	s := settings("pve1")
	s.ReleaseMirror = &ReleaseMirror{URL: "https://mirror.example.org/github", CABundle: validAuthority(t)}
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "release_mirror         = var.settings.releaseMirror") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(vars), `"url": "https://mirror.example.org/github"`) {
		t.Fatalf("tfvars without the release mirror: %v\n%s", err, vars)
	}
	if got := moduleReleaseMirror(nil); got["url"] != "" || got[moduleCABundle] != "" {
		t.Errorf("no setting gave %v", got)
	}
}
