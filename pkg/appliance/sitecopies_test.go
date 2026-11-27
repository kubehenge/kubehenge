package appliance

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSiteBundleAnchorsAnIntermediateThatALeafNamesInOtherBytes accepts what OpenSSL accepts where
// the leaf names its intermediate in other bytes than the intermediate's subject. The anchor
// expires with the root and stays in the bundle of a later run. A root that constrains names, which
// the anchor would drop, keeps the chain refused.
func TestSiteBundleAnchorsAnIntermediateThatALeafNamesInOtherBytes(t *testing.T) {
	t.Parallel()
	rootEnd := time.Now().Add(6 * time.Hour).Truncate(time.Second)
	root := newTestRoot(t, "Inspect Root", func(c *x509.Certificate) { c.NotAfter = rootEnd })
	intermediate := root.issue(t, issuance{cn: "Inspect Intermediate", tag: asn1.TagPrintableString})
	leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true, issuerTag: asn1.TagUTF8String})
	chain := certs(leaf, intermediate)
	if goTrusts(chain, root.pem()) == nil {
		t.Fatal("Go accepted the chain with the pasted root alone, so the chain proves nothing")
	}
	bundle, err := bundleFor(root, leaf, intermediate)
	if err != nil {
		t.Fatal(err)
	}
	if err = goTrusts(chain, bundle); err != nil {
		t.Errorf("the bundle does not make Go accept the chain: %v", err)
	}
	held, err := parseCertificates(bundle)
	if err != nil {
		t.Fatal(err)
	}
	anchor := held[len(held)-1]
	if len(held) != 2 || !anchor.NotAfter.Equal(rootEnd) || !bytes.Equal(anchor.RawIssuer, leaf.cert.RawIssuer) || !bytes.Equal(anchor.RawSubject, leaf.cert.RawIssuer) || len(anchor.AuthorityKeyId) != 0 {
		t.Errorf("the bundle holds %d certificates, the last named %q by %q until %s with authority key ID %x", len(held), anchor.Subject, anchor.Issuer, anchor.NotAfter, anchor.AuthorityKeyId)
	}
	auth, err := newSiteAuthority(siteCALabel, root.pem(), bundle, new(string))
	if err != nil || auth.derived() != bundle {
		t.Errorf("a later run with the same Site CA keeps %q, %v", auth.derived(), err)
	}

	constrained := newTestRoot(t, "Inspect Root", func(c *x509.Certificate) { c.PermittedDNSDomains = []string{"host.example"} })
	under := constrained.issue(t, issuance{cn: "Inspect Intermediate", tag: asn1.TagPrintableString})
	underLeaf := under.issue(t, issuance{cn: "host.example", leaf: true, issuerTag: asn1.TagUTF8String})
	if _, err = bundleFor(constrained, underLeaf, under); err == nil || !strings.Contains(err.Error(), "encodes its issuer's name") {
		t.Errorf("a root that constrains names gave %v", err)
	}
}

// TestASaveThatKeepsTheSiteCAKeepsTheCopiesTheOtherHostsNeed saves a Site CA while no host answers,
// then again while one host needs a copy and another presents an expired intermediate. The nodes
// get the copy, though the fault keeps the last run's bundle, which is empty.
func TestASaveThatKeepsTheSiteCAKeepsTheCopiesTheOtherHostsNeed(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	old := c.root.issue(t, issuance{cn: "Inspect Old", issuerTag: asn1.TagUTF8String, notAfter: time.Now().Add(-time.Minute)})
	a, ready := newAppliance(t)
	a.sitePublic = x509.NewCertPool()
	a.SiteChains = fakeChains(nil)
	apply := func(s *Settings) *JobView {
		t.Helper()
		if err := a.Apply(s, "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
			t.Fatal(err)
		}
		j := waitFor(t, a)
		<-ready
		return j
	}
	s := settings("pve1")
	s.SiteCA = c.root.pem()
	apply(s)
	if saved := a.SavedSettings(); saved.SiteBundle != "" {
		t.Fatalf("a run where no host answered saved the bundle %q", saved.SiteBundle)
	}

	good := certs(c.leaf, c.intermediate)
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": good, "registry.k8s.io": certs(old.issue(t, issuance{cn: "host.example", leaf: true}), old)})
	again := settings("pve1")
	again.SiteCA, again.TimeZone = c.root.pem(), "Europe/Zurich"
	if j := apply(again); j.State != StateSucceeded || goTrusts(good, moduleSiteCAOf(t, a)) != nil {
		t.Errorf("the run (%s) gave the nodes %q", j.State, moduleSiteCAOf(t, a))
	}
}

// TestAFirstSiteCheckReadsThroughAnHTTPSProxyThatNeedsACopy reads the hosts through an https proxy
// whose own chain names the pasted root in other bytes, with nothing saved before.
func TestAFirstSiteCheckReadsThroughAnHTTPSProxyThatNeedsACopy(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	handler, asked := connectHandler(tlsHost(t, c.leaf, c.intermediate))
	proxy := httptest.NewUnstartedServer(handler)
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{c.leaf.cert.Raw, c.intermediate.cert.Raw}, PrivateKey: c.leaf.key}}}
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	a := &Appliance{Dir: t.TempDir(), ProxmoxURL: "https://10.0.0.50:8006", sitePublic: x509.NewCertPool()}
	a.SiteChains = a.DialChain
	s := &Settings{SiteCA: c.root.pem(), Proxy: &Proxy{URL: proxy.URL}}
	unchecked, err := a.ResolveSite(t.Context(), s)
	if err != nil || len(unchecked) != 0 || goTrusts(certs(c.leaf, c.intermediate), s.siteTrust()) != nil {
		t.Errorf("unchecked %q, %v, bundle %q", unchecked, err, s.SiteBundle)
	}
	if got := asked(); len(got) != len(s.siteHosts()) {
		t.Errorf("the proxy was asked %q", got)
	}
}

// TestAVisibleStringIssuerIsRefusedAndNamedAsText refuses an intermediate that names its root in
// VisibleString, which OpenSSL cannot load, though the rest of the name differs only in its string
// type. The fault shows the name as text.
func TestAVisibleStringIssuerIsRefusedAndNamedAsText(t *testing.T) {
	t.Parallel()
	name := func(organisationTag int) []byte {
		der, err := asn1.Marshal(pkix.RDNSequence{
			{{Type: oidCommonName, Value: asn1.RawValue{Class: asn1.ClassUniversal, Tag: tagVisibleString, Bytes: []byte("Inspect Root")}}},
			{{Type: asn1.ObjectIdentifier{2, 5, 4, 10}, Value: asn1.RawValue{Class: asn1.ClassUniversal, Tag: organisationTag, Bytes: []byte("Corp")}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return der
	}
	root := newTestRoot(t, "Inspect Root", func(c *x509.Certificate) { c.RawSubject = name(asn1.TagPrintableString) })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), RawSubject: nameDER(t, "Inspect Intermediate", asn1.TagUTF8String), SubjectKeyId: []byte("Inspect Intermediate"),
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	parent := &x509.Certificate{RawSubject: name(asn1.TagUTF8String), SubjectKeyId: root.cert.SubjectKeyId}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, root.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	intermediate := &testAuthority{cn: "Inspect Intermediate", tag: asn1.TagUTF8String, key: key, cert: cert}
	leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
	_, err = bundleFor(root, leaf, intermediate)
	if err == nil || !strings.Contains(err.Error(), "CN=Inspect Root") || strings.Contains(err.Error(), "{") {
		t.Errorf("a VisibleString issuer gave %v", err)
	}
}

func TestTheRunLogNamesTheProxyCertificateWhereItAloneIsChecked(t *testing.T) {
	t.Parallel()
	j := &Job{}
	noteSite(j, &Settings{Proxy: &Proxy{URL: "http://proxy.example:3128", CABundle: newTestRoot(t, "Proxy Root").pem()}}, []string{"ghcr.io"}, SiteChainError{})
	if got := j.Log(); !slices.Equal(got, []string{"The proxy certificate was not checked against ghcr.io. They did not answer."}) {
		t.Errorf("the log says %q", got)
	}
}

// TestASaveThatChangesOnlyTheProxyCARefusesAChainFault refuses a chain fault where the Site CA stays
// as saved and the proxy's CA changes, as a changed authority has no last run to go on with.
func TestASaveThatChangesOnlyTheProxyCARefusesAChainFault(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	site := newTestRoot(t, "Site Root").pem()
	a := &Appliance{Dir: t.TempDir(), sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(c.leaf)})}
	saved := &Settings{SiteCA: site, Proxy: &Proxy{URL: "http://proxy.example:3128", CABundle: newTestRoot(t, "Old Proxy Root").pem()}}
	if err := os.MkdirAll(a.workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(a.workdir(), "settings.json"), saved); err != nil {
		t.Fatal(err)
	}
	s := &Settings{SiteCA: site, Proxy: &Proxy{URL: "http://proxy.example:3128", CABundle: c.root.pem()}}
	got := a.CheckSite(t.Context(), s)
	if len(got.Errors) != 1 || !strings.HasPrefix(got.Errors[0], "Proxy certificate: ghcr.io. ") || len(got.Warnings) != 0 {
		t.Errorf("errors %q, warnings %q", got.Errors, got.Warnings)
	}
}
