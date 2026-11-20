package ui

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

func caOf(t *testing.T, v *applianceCAView) *x509.Certificate {
	t.Helper()
	if v == nil {
		t.Fatal("the answer shows no CA")
	}
	block, _ := pem.Decode([]byte(v.PEM))
	if block == nil {
		t.Fatal("the CA's PEM does not read")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func restartedAt(t *testing.T, c *InterfaceCertificate, at time.Time) *InterfaceCertificate {
	t.Helper()
	next := NewInterfaceCertificate(c.dir, c.certFile, c.keyFile)
	next.Roots = x509.NewCertPool()
	if err := next.Load(at, nil); err != nil {
		t.Fatal(err)
	}
	return next
}

// The appliance's CA signs a certificate for the names and addresses of the one served, and a
// browser that trusts it accepts that certificate. The CA stays for the next one, and comes back
// after a restart.
func TestTheAppliancesCASignsTheServedNames(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	v := certificateOf(t, a.do("POST", "/api/appliance/certificate/appliance-ca", ""))
	ca := caOf(t, v.ApplianceCA)
	leaf := c.served().pair.Leaf
	if v.Source != certificateApplianceCA || v.SelfSigned || v.Fingerprint != appliance.Fingerprint(leaf) || v.Incomplete {
		t.Errorf("view = %+v", v)
	}
	if got := strings.Join(v.ApplianceCA.Permits, ","); got != certificateHost+",kubehenge,192.0.2.40" {
		t.Errorf("the CA permits %s", got)
	}
	if !ca.IsCA || !ca.MaxPathLenZero || ca.NotAfter.Sub(ca.NotBefore) < 9*365*24*time.Hour || ca.Subject.CommonName != "Kubehenge appliance CA" {
		t.Errorf("the CA is %+v", ca)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, name := range []string{certificateHost, "kubehenge", "192.0.2.40"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, CurrentTime: h.clock}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if chain := c.served().chain; len(chain) != 2 || !chain[1].Equal(ca) {
		t.Errorf("the chain served is %d long", len(chain))
	}
	raw, err := os.ReadFile(filepath.Join(c.dir, appliance.InterfaceCAFile))
	if err != nil || !strings.HasPrefix(string(raw), "-----BEGIN PRIVATE KEY-----") {
		t.Errorf("the CA's file: %v", err)
	}

	again := certificateOf(t, a.do("POST", "/api/appliance/certificate/appliance-ca", ""))
	if again.ApplianceCA.Fingerprint != v.ApplianceCA.Fingerprint || again.Fingerprint == v.Fingerprint {
		t.Errorf("signing again made a new CA, or kept the certificate: %+v", again)
	}
	next := restartedAt(t, c, h.clock.AddDate(0, 0, 1))
	if next.served().source != certificateApplianceCA || next.fingerprint() != again.Fingerprint {
		t.Errorf("after a restart: %s from %s", next.fingerprint(), next.served().source)
	}
	if shown := certificateOf(t, a.do("GET", "/api/appliance/certificate", "")); shown.ApplianceCA == nil || shown.ApplianceCA.Fingerprint != v.ApplianceCA.Fingerprint {
		t.Errorf("the page shows %+v", shown.ApplianceCA)
	}
}

// Names the CA may not sign for make a new CA, held to the new names.
func TestNewNamesMakeANewApplianceCA(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	first := caOf(t, certificateOf(t, a.do("POST", "/api/appliance/certificate/appliance-ca", "")).ApplianceCA)
	moved := issueCertificate(t, h.clock.Add(-time.Hour), h.clock.AddDate(1, 0, 0), []string{"moved.example.org"}, "198.51.100.7")
	c.live.Store(&servedCertificate{pair: tlsPair(t, moved), source: certificateUploaded})
	v := certificateOf(t, a.do("POST", "/api/appliance/certificate/appliance-ca", ""))
	if second := caOf(t, v.ApplianceCA); second.Equal(first) || strings.Join(v.ApplianceCA.Permits, ",") != "moved.example.org,198.51.100.7" {
		t.Errorf("the CA after a move permits %v", v.ApplianceCA.Permits)
	}
}

// A CA vouches only for its names and addresses. One made for names alone signs for no address, and
// one made for addresses alone signs for no name.
func TestTheAppliancesCAVouchesForNothingElse(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		names []string
		ips   []string
		other *x509.Certificate
	}{
		{[]string{certificateHost}, nil, &x509.Certificate{DNSNames: []string{"bank.example.net"}}},
		{[]string{certificateHost}, nil, &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("198.51.100.1")}}},
		{[]string{certificateHost}, nil, &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("2001:db8::1")}}},
		{nil, []string{"192.0.2.40"}, &x509.Certificate{DNSNames: []string{"bank.example.net"}}},
		{nil, []string{"192.0.2.40"}, &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("192.0.2.41")}}},
		{nil, []string{"192.0.2.40"}, &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("2001:db8::1")}}},
		{nil, []string{"2001:db8::40"}, &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("192.0.2.40")}}},
	} {
		own := &x509.Certificate{DNSNames: c.names}
		for _, ip := range c.ips {
			own.IPAddresses = append(own.IPAddresses, net.ParseIP(ip))
		}
		ca, err := newApplianceCA(own, now)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ca.sign(own, now)
		if err != nil || !ca.vouches(b.Leaf, now) {
			t.Errorf("%v %v: its own certificate: %v", c.names, c.ips, err)
		}
		if b, err = ca.sign(c.other, now); err != nil || ca.vouches(b.Leaf, now) {
			t.Errorf("%v %v vouches for %v %v (%v)", c.names, c.ips, c.other.DNSNames, c.other.IPAddresses, err)
		}
	}
}

// A certificate of the appliance's CA is signed again when the interface starts with under a third
// of its lifetime left, by the same CA.
func TestTheAppliancesCertificateIsSignedAgainWhenAThirdIsLeft(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	v := certificateOf(t, a.do("POST", "/api/appliance/certificate/appliance-ca", ""))
	if early := restartedAt(t, c, h.clock.AddDate(0, 0, 500)); early.fingerprint() != v.Fingerprint {
		t.Error("signed again with two thirds left")
	}
	late := restartedAt(t, c, h.clock.AddDate(0, 0, 600))
	leaf := late.served().pair.Leaf
	if late.fingerprint() == v.Fingerprint || late.served().source != certificateApplianceCA || leaf.NotBefore.Before(h.clock.AddDate(0, 0, 599)) {
		t.Errorf("not signed again: %s from %s, from %s", late.fingerprint(), late.served().source, leaf.NotBefore)
	}
	if !slices.Equal(leaf.DNSNames, []string{certificateHost, "kubehenge"}) || appliance.Fingerprint(late.served().chain[1]) != v.ApplianceCA.Fingerprint {
		t.Errorf("signed again for %v, by a new CA", leaf.DNSNames)
	}
	if kept := restartedAt(t, c, h.clock.AddDate(0, 0, 601)); kept.fingerprint() != late.fingerprint() {
		t.Error("the certificate signed again was not kept")
	}
}

// A reset forgets the CA with the certificate. A certificate that names nothing gives the CA
// nothing to sign for.
func TestAResetForgetsTheAppliancesCA(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	certificateOf(t, a.do("POST", "/api/appliance/certificate/appliance-ca", ""))
	if v := certificateOf(t, a.do("DELETE", "/api/appliance/certificate", "")); v.ApplianceCA != nil {
		t.Errorf("the CA is still shown: %+v", v.ApplianceCA)
	}
	if got := storedFiles(t, c); len(got) != 0 {
		t.Errorf("the appliance directory holds %v", got)
	}
	nameless := issueCertificate(t, h.clock.Add(-time.Hour), h.clock.AddDate(1, 0, 0), nil)
	c.live.Store(&servedCertificate{pair: tlsPair(t, nameless), source: certificateInstaller})
	refusal(t, a.do("POST", "/api/appliance/certificate/appliance-ca", ""), http.StatusUnprocessableEntity, "no-names")
	if got := storedFiles(t, c); len(got) != 0 {
		t.Errorf("a refusal left %v", got)
	}
}
