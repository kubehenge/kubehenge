package appliance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

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
