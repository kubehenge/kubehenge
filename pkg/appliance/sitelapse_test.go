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
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// saveLapsedSiteCA applies settings with no site CA and then has the saved ones carry a site CA
// that has since expired, as a site CA does that nobody replaced.
func saveLapsedSiteCA(t *testing.T, a *Appliance, ready chan []byte) *Settings {
	t.Helper()
	a.Release = "0.1.0"
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	<-ready
	saved := a.SavedSettings()
	saved.SiteCA = lapsedAuthority(t)
	if err := writeJSONFile(filepath.Join(a.workdir(), "settings.json"), saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestUnattendedUpgradeRunsOnASavedSiteCAThatHasLapsed(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	saveLapsedSiteCA(t, a, ready)
	a.Release = "0.2.0"
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyUnattended(func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v, want the upgrade to run on the lapsed site CA", j)
	}
}

func TestCheckAcceptsTheSavedSiteCAThatHasLapsedAndRefusesANewOne(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	saved := saveLapsedSiteCA(t, a, ready)
	same := settings("pve1")
	same.SiteCA = saved.SiteCA
	if err := a.Check(same); err != nil {
		t.Errorf("the saved site CA, run out since, stopped the check: %v", err)
	}
	other := settings("pve1")
	other.SiteCA = lapsedAuthority(t)
	if err := a.Check(other); err == nil {
		t.Error("a different site CA that has run out passed the check")
	}
}

// lapsedAuthority is a CA certificate that expired a minute ago.
func lapsedAuthority(t *testing.T) string {
	t.Helper()
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
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestSavedSiteCAThatLapsedStopsNoApplyButANewOneMustBeValid(t *testing.T) {
	t.Parallel()
	lapsed := lapsedAuthority(t)
	saved := settings("pve1")
	saved.SiteCA = lapsed
	next := settings("pve1")
	next.SiteCA = lapsed
	if err := next.ValidateAgainst(saved); err != nil {
		t.Errorf("the saved site CA, run out since, stopped the apply: %v", err)
	}
	if err := saved.ValidateAgainst(saved); err != nil {
		t.Errorf("the saved site CA, run out since, stopped the unattended upgrade: %v", err)
	}
	changed := settings("pve1")
	changed.SiteCA = lapsed
	other := settings("pve1")
	other.SiteCA = validAuthority(t)
	if err := changed.ValidateAgainst(other); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("a site CA that is new and expired passed: %v", err)
	}
	fresh := settings("pve1")
	fresh.SiteCA = lapsed
	if err := fresh.ValidateAgainst(nil); err == nil {
		t.Error("a first site CA that is expired passed")
	}
}
