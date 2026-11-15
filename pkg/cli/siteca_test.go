package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
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
	// The mirror's authority is read per cluster, from the site CA each was given, so the configured
	// mirror stays without one and a new site CA reaches the nodes through the paced rollout alone.
	if o.releaseMirror.CABundle != "" || o.r.SiteCA != strings.TrimSpace(site) {
		t.Errorf("a mirror without an authority: %q, site CA %q, want none and the site CA", o.releaseMirror.CABundle, o.r.SiteCA)
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

	// The controller's own reads of the mirror are verified with the site CA.
	o = &controllerOptions{siteCAFile: sitePath}
	o.releaseMirror.URL = "https://mirror.example.org"
	if err := o.parseReleaseMirror(); err != nil {
		t.Fatal(err)
	}
	if got := o.releaseChecksums(nil).DefaultMirror().CABundle; got != strings.TrimSpace(site) {
		t.Errorf("the controller reads the mirror with %q, want the site CA", got)
	}

	o = &controllerOptions{siteCAFile: sitePath}
	o.releaseMirror.URL = "http://mirror.example.org"
	if err := o.parseReleaseMirror(); err != nil || o.releaseMirror.CABundle != "" {
		t.Errorf("a plain HTTP mirror: %v, %q", err, o.releaseMirror.CABundle)
	}
}

func TestControllerTakesTheSiteCAAndTimeZoneAsFlags(t *testing.T) {
	t.Parallel()
	cmd := newControllerCommand(&pve.Config{})
	if f := cmd.Flags().Lookup("site-ca"); f == nil || f.DefValue != "" {
		t.Errorf("--site-ca = %v, want a flag with no default", f)
	}
	if f := cmd.Flags().Lookup("time-zone"); f == nil || f.DefValue != "UTC" {
		t.Errorf("--time-zone = %v, want a flag defaulting to UTC", f)
	}
}

func TestServedReleaseMirrorFollowsTheSavedSiteCA(t *testing.T) {
	t.Parallel()
	_, site := authorityFile(t, "site")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "management"), 0o700); err != nil {
		t.Fatal(err)
	}
	saved, err := json.Marshal(map[string]any{"releaseMirror": map[string]any{"url": "https://mirror.example.org"}, "siteCA": site})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "management", "settings.json"), saved, 0o600); err != nil {
		t.Fatal(err)
	}
	o := &serveOptions{}
	o.a.Dir = dir
	if got := o.savedReleaseMirror(); got.URL != "https://mirror.example.org" || got.CABundle != site {
		t.Errorf("the saved release mirror = %+v, want the site CA as its authority", got)
	}
}

func TestControllerReportsABadSiteCAAndTimeZoneAndRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	bad := filepath.Join(t.TempDir(), "bad.crt")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file without a certificate stops no controller. It is reported on each cluster and reaches
	// no node.
	o := &controllerOptions{siteCAFile: bad}
	if err := o.parseReleaseMirror(); err != nil || o.r.SiteCA != "" || !strings.Contains(o.r.SiteCAProblem, "--site-ca") ||
		!strings.Contains(o.r.SiteCAProblem, "holds no PEM certificate") {
		t.Errorf("a file without a certificate gave %v, site CA %q, problem %q", err, o.r.SiteCA, o.r.SiteCAProblem)
	}
	o = &controllerOptions{siteCAFile: filepath.Join(t.TempDir(), "missing")}
	if err := o.parseReleaseMirror(); err == nil || !strings.Contains(err.Error(), "--site-ca") {
		t.Errorf("a missing file gave %v", err)
	}
	// A zone that is none stops no controller. It is reported on each cluster instead.
	o = &controllerOptions{timeZone: "Mars/Olympus"}
	if err := o.parseReleaseMirror(); err != nil || !strings.Contains(o.r.SiteTimeZoneProblem, "--time-zone") {
		t.Errorf("a time zone that is none gave %v with the problem %q", err, o.r.SiteTimeZoneProblem)
	}
}
