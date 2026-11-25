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
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

var oidCommonName = asn1.ObjectIdentifier{2, 5, 4, 3}

// nameDER encodes a name of one common name as the given ASN.1 string type, which is how two
// encodings of one name differ for Go and not for OpenSSL.
func nameDER(t *testing.T, cn string, tag int) []byte {
	t.Helper()
	der, err := asn1.Marshal(pkix.RDNSequence{{{Type: oidCommonName, Value: asn1.RawValue{Class: asn1.ClassUniversal, Tag: tag, Bytes: []byte(cn)}}}})
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// testAuthority is a generated certificate with its key, and the encoding it names itself with.
type testAuthority struct {
	cn   string
	tag  int
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

func (a *testAuthority) pem() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw}))
}

// newTestRoot is a self-signed root, each change applied to its template before it is signed.
func newTestRoot(t *testing.T, cn string, changes ...func(*x509.Certificate)) *testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), RawSubject: nameDER(t, cn, asn1.TagPrintableString), SubjectKeyId: []byte(cn),
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	for _, change := range changes {
		change(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testAuthority{cn: cn, tag: asn1.TagPrintableString, key: key, cert: cert}
}

// issuance is what a certificate is issued with. The zero value is an intermediate in UTF8String,
// valid now, naming its issuer as the issuer names itself.
type issuance struct {
	cn        string
	tag       int
	issuerTag int
	leaf      bool
	// notCA and noSign make an intermediate that may not sign.
	notCA, noSign bool
	notAfter      time.Time
	sig           x509.SignatureAlgorithm
}

func (a *testAuthority) issue(t *testing.T, o issuance) *testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	o.tag = cmpOr(o.tag, asn1.TagUTF8String)
	o.issuerTag = cmpOr(o.issuerTag, a.tag)
	if o.notAfter.IsZero() {
		o.notAfter = time.Now().Add(12 * time.Hour)
	}
	parent := &x509.Certificate{RawSubject: nameDER(t, a.cn, o.issuerTag), SubjectKeyId: a.cert.SubjectKeyId}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), RawSubject: nameDER(t, o.cn, o.tag), SubjectKeyId: []byte(o.cn),
		NotBefore: time.Now().Add(-time.Hour), NotAfter: o.notAfter, SignatureAlgorithm: o.sig,
		IsCA: !o.leaf && !o.notCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	if o.noSign {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	}
	if o.leaf {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
		tmpl.DNSNames = []string{"host.example"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testAuthority{cn: o.cn, tag: o.tag, key: key, cert: cert}
}

func cmpOr(v, fallback int) int {
	if v != 0 {
		return v
	}
	return fallback
}

// inspected is a root in PrintableString, an intermediate that names it in UTF8String, and a leaf
// the intermediate signs: the chain of an inspection device, which curl accepts and Go refuses.
type inspected struct {
	root, intermediate, leaf *testAuthority
}

func newInspected(t *testing.T) inspected {
	t.Helper()
	root := newTestRoot(t, "Inspect Root")
	intermediate := root.issue(t, issuance{cn: "Inspect Intermediate", issuerTag: asn1.TagUTF8String})
	leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
	return inspected{root, intermediate, leaf}
}

// goTrusts verifies the chain as a node does, with the bundle's certificates as its roots and the
// chain's own intermediates.
func goTrusts(chain []*x509.Certificate, bundle string) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(bundle)) {
		return errors.New("the bundle holds no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err
}

func certs(authorities ...*testAuthority) []*x509.Certificate {
	out := make([]*x509.Certificate, 0, len(authorities))
	for _, a := range authorities {
		out = append(out, a.cert)
	}
	return out
}

// bundleFor is the bundle for one host's chain with the root pasted and no public authority.
func bundleFor(root *testAuthority, chain ...*testAuthority) (string, error) {
	auth, err := newSiteAuthority(siteCALabel, root.pem(), "", new(string))
	if err != nil {
		return "", err
	}
	if err = judgeChains([]*siteAuthority{auth}, x509.NewCertPool(), []hostChain{{host: "host.example", chain: certs(chain...)}}, time.Now()); err != nil {
		return "", err
	}
	if bundle := auth.derived(); bundle != "" {
		return bundle, nil
	}
	return root.pem(), nil
}

func TestSiteBundleMakesGoAcceptWhatOpenSSLAcceptsAcrossARenamedRoot(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	chain := certs(c.leaf, c.intermediate)
	if err := goTrusts(chain, c.root.pem()); err == nil {
		t.Fatal("Go accepted the chain with the pasted root alone, so the chain proves nothing")
	}
	bundle, err := bundleFor(c.root, c.leaf, c.intermediate)
	if err != nil {
		t.Fatal(err)
	}
	if err = goTrusts(chain, bundle); err != nil {
		t.Errorf("the bundle does not make Go accept the chain: %v", err)
	}
	if !strings.HasPrefix(bundle, c.root.pem()) {
		t.Error("the bundle does not open with the pasted root as it was pasted")
	}
}

func TestSiteBundleCopiesARootThatALeafNamesInOtherBytes(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "Inspect Root")
	leaf := root.issue(t, issuance{cn: "host.example", leaf: true, issuerTag: asn1.TagUTF8String})
	if err := goTrusts(certs(leaf), root.pem()); err == nil {
		t.Fatal("Go accepted the chain with the pasted root alone, so the chain proves nothing")
	}
	bundle, err := bundleFor(root, leaf)
	if err != nil {
		t.Fatal(err)
	}
	if err = goTrusts(certs(leaf), bundle); err != nil {
		t.Errorf("the re-encoded root does not make Go accept the leaf: %v", err)
	}
	copies, err := parseCertificates(bundle)
	if err != nil {
		t.Fatal(err)
	}
	renamed := copies[len(copies)-1]
	if key, ok := renamed.PublicKey.(*ecdsa.PublicKey); !ok || !key.Equal(root.key.Public()) || len(copies) != 2 {
		t.Errorf("the bundle holds %d certificates, the last of them not the root's key", len(copies))
	}
}

func TestSiteBundleTakesThreeIntermediatesInAnyOrder(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "Inspect Root")
	first := root.issue(t, issuance{cn: "Inspect One", issuerTag: asn1.TagUTF8String})
	second := first.issue(t, issuance{cn: "Inspect Two", tag: asn1.TagPrintableString, issuerTag: asn1.TagUTF8String})
	third := second.issue(t, issuance{cn: "Inspect Three"})
	leaf := third.issue(t, issuance{cn: "host.example", leaf: true})
	for name, presented := range map[string][]*testAuthority{
		"in order":     {leaf, third, second, first},
		"out of order": {leaf, first, third, second},
	} {
		bundle, err := bundleFor(root, presented...)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if err = goTrusts(certs(presented...), bundle); err != nil {
			t.Errorf("%s: the bundle does not make Go accept the chain: %v", name, err)
		}
	}
}

func TestSiteBundleNamesAnIntermediateTheHostDoesNotPresent(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	_, err := bundleFor(c.root, c.leaf)
	if err == nil || err.Error() != `Site CA: host.example: "CN=Inspect Intermediate" is missing. Paste it with the root.` {
		t.Errorf("a chain missing its intermediate: %v", err)
	}
}

func TestSiteBundleNamesASignatureGoRefuses(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "Inspect Root")
	weak := root.issue(t, issuance{cn: "Inspect Weak", sig: x509.ECDSAWithSHA1})
	leaf := weak.issue(t, issuance{cn: "host.example", leaf: true})
	_, err := bundleFor(root, leaf, weak)
	if err == nil || !strings.Contains(err.Error(), "CN=Inspect Weak") || !strings.Contains(err.Error(), "ECDSA-SHA1") {
		t.Errorf("an intermediate signed with SHA-1: %v", err)
	}
}

func TestSiteBundleNamesAnExpiredIntermediate(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "Inspect Root")
	old := root.issue(t, issuance{cn: "Inspect Old", notAfter: time.Now().Add(-time.Minute)})
	leaf := old.issue(t, issuance{cn: "host.example", leaf: true, notAfter: time.Now().Add(time.Hour)})
	_, err := bundleFor(root, leaf, old)
	if err == nil || !strings.Contains(err.Error(), "CN=Inspect Old") || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an expired intermediate: %v", err)
	}
}

// fakeChains answers each host with its chain, or fails it where the chain is nil.
func fakeChains(chains map[string][]*x509.Certificate) SiteChains {
	return func(_ context.Context, _ *Settings, host string) ([]*x509.Certificate, error) {
		if chain := chains[host]; chain != nil {
			return chain, nil
		}
		return nil, errors.New("no answer")
	}
}

func TestSiteCheckListsAHostThatDoesNotAnswerAndKeepsTheOthers(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(c.leaf, c.intermediate)})}
	s := &Settings{SiteCA: c.root.pem()}
	unchecked, err := a.ResolveSite(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"factory.talos.dev", "tuf-repo-cdn.sigstore.dev"}; !slices.Equal(unchecked, want) {
		t.Errorf("unchecked %v, want %v", unchecked, want)
	}
	if s.SiteBundle == "" || goTrusts(certs(c.leaf, c.intermediate), s.siteTrust()) != nil {
		t.Errorf("the bundle %q does not hold what the host that answered needs", s.SiteBundle)
	}
}

func TestSiteCheckAddsNothingForAChainThatAPublicAuthoritySigns(t *testing.T) {
	t.Parallel()
	public := newTestRoot(t, "Public Root")
	issuer := public.issue(t, issuance{cn: "Public Intermediate"})
	leaf := issuer.issue(t, issuance{cn: "host.example", leaf: true})
	pool := x509.NewCertPool()
	pool.AddCert(public.cert)
	inspection := newTestRoot(t, "Inspect Root")
	chain := certs(leaf, issuer)
	a := &Appliance{sitePublic: pool, SiteChains: fakeChains(map[string][]*x509.Certificate{"factory.talos.dev": chain, "ghcr.io": chain, "tuf-repo-cdn.sigstore.dev": chain})}
	s := &Settings{SiteCA: inspection.pem()}
	unchecked, err := a.ResolveSite(t.Context(), s)
	if err != nil || len(unchecked) != 0 || s.SiteBundle != "" || s.siteTrust() != s.SiteCA {
		t.Errorf("a public chain gave %v, unchecked %v, bundle %q", err, unchecked, s.SiteBundle)
	}
}

// TestSavedSiteCAIsWhatWasPastedAndTheBundleReachesEveryConsumer applies a Site CA whose chain Go
// refuses, then reads each place the Site CA went before.
func TestSavedSiteCAIsWhatWasPastedAndTheBundleReachesEveryConsumer(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{c.leaf.cert.Raw, c.intermediate.cert.Raw}, PrivateKey: c.leaf.key}}}
	server.StartTLS()
	t.Cleanup(server.Close)

	a, _ := newAppliance(t)
	chain := certs(c.leaf, c.intermediate)
	a.sitePublic = x509.NewCertPool()
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"factory.talos.dev": chain, "ghcr.io": chain, "tuf-repo-cdn.sigstore.dev": chain, "mirror.example.org": chain})
	s := settings("pve1")
	s.SiteCA = c.root.pem()
	s.ReleaseMirror = &ReleaseMirror{URL: "https://mirror.example.org"}
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("job = %+v", j)
	}
	saved := a.SavedSettings()
	if saved.SiteCA != c.root.pem() {
		t.Errorf("the saved Site CA is %q, want what was pasted", saved.SiteCA)
	}
	bundle := saved.SiteBundle
	if bundle == "" || bundle == saved.SiteCA || !strings.HasPrefix(bundle, saved.SiteCA) {
		t.Fatalf("the saved bundle is %q beside the Site CA %q", bundle, saved.SiteCA)
	}
	raw, err := os.ReadFile(filepath.Join(a.workdir(), "terraform.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vars struct {
		Settings moduleSiteCA `json:"settings"`
	}
	if err = json.Unmarshal(raw, &vars); err != nil || vars.Settings.SiteCA != bundle {
		t.Errorf("the module, and so the nodes' trusted roots and the controller's Site CA, got %q (%v), want the bundle", vars.Settings.SiteCA, err)
	}
	outbound, err := os.ReadFile(filepath.Join(a.workdir(), "outbound-ca.pem"))
	if err != nil || string(outbound) != bundle || saved.OutboundCA() != bundle {
		t.Errorf("the downloads' authority is %q (%v), want the bundle", outbound, err)
	}
	if got := saved.EffectiveReleaseMirror().CABundle; got != bundle {
		t.Errorf("the release mirror trusts %q, want the bundle", got)
	}
	if got := a.siteCA(); got != bundle {
		t.Errorf("the backup targets trust %q, want the bundle", got)
	}
	resp, err := (&http.Client{Transport: &SavedTransport{Dir: a.Dir}, Timeout: 10 * time.Second}).Get(server.URL) //nolint:noctx // a test server
	if err != nil {
		t.Errorf("a download from a host behind the chain: %v", err)
	} else {
		_ = resp.Body.Close()
	}
}

// moduleSiteCA is the part of the module's variables that holds the Site CA.
type moduleSiteCA struct {
	SiteCA string `json:"siteCA"`
}

func TestSiteNoteSaysWhereTheChainsWereReadFrom(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		own  []string
		want string
	}{
		"inside the nodes' network":  {[]string{"10.0.0.5"}, "The Site CA was checked from the appliance at 10.0.0.5"},
		"outside the nodes' network": {[]string{"192.168.9.5"}, "The Site CA was checked from the appliance at 192.168.9.5\nThe management nodes' network was not checked"},
		"address unknown":            {nil, ""},
	} {
		a := &Appliance{SiteChains: fakeChains(nil), Addresses: func() ([]string, error) { return tc.own, nil }}
		s := settings("pve1")
		s.SiteCA = "x"
		j := &Job{}
		a.noteSite(j, s, nil)
		if got := strings.Join(j.Log(), "\n"); got != tc.want {
			t.Errorf("%s: log %q, want %q", name, got, tc.want)
		}
	}
}

// TestSiteBundleRefusesWhatTheRenamedRootsLimitsRefuse gives the nodes only renamed copies of the
// pasted root, never a presented intermediate, so that Go still applies the root's path length, name
// constraints and validity.
func TestSiteBundleRefusesWhatTheRenamedRootsLimitsRefuse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		root func(*x509.Certificate)
		want string
	}{
		"path length": {
			root: func(c *x509.Certificate) { c.MaxPathLen, c.MaxPathLenZero = 0, true },
			want: `"CN=Inspect Root" allows fewer intermediates than the host presents`,
		},
		"name constraints": {
			root: func(c *x509.Certificate) { c.PermittedDNSDomains = []string{"corp.example"} },
			want: `"CN=host.example" names a host its issuers may not sign for`,
		},
		"expired root": {
			root: func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Minute) },
			want: `"CN=Inspect Root" expired on`,
		},
	} {
		root := newTestRoot(t, "Inspect Root", tc.root)
		intermediate := root.issue(t, issuance{cn: "Inspect Intermediate", issuerTag: asn1.TagUTF8String})
		leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
		if _, err := bundleFor(root, leaf, intermediate); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to hold %q", name, err, tc.want)
		}
	}
}

func TestSiteBundleHoldsOnlyThePastedRootsAndTheirRenamedCopies(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	bundle, err := bundleFor(c.root, c.leaf, c.intermediate)
	if err != nil {
		t.Fatal(err)
	}
	held, err := parseCertificates(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		if !bytes.Equal(h.RawSubjectPublicKeyInfo, c.root.cert.RawSubjectPublicKeyInfo) {
			t.Errorf("the bundle holds %q, which is no copy of the pasted root", h.Subject)
		}
	}
}

// TestSiteBundleNamesTheCertificateThatBreaksTheChain names the issuer no presented certificate
// matches, and an intermediate that may not sign, rather than the leaf's issuer each time.
func TestSiteBundleNamesTheCertificateThatBreaksTheChain(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "Inspect Root")
	one := root.issue(t, issuance{cn: "Inspect One", issuerTag: asn1.TagUTF8String})
	two := one.issue(t, issuance{cn: "Inspect Two"})
	three := two.issue(t, issuance{cn: "Inspect Three"})
	leaf := three.issue(t, issuance{cn: "host.example", leaf: true})
	notCA := root.issue(t, issuance{cn: "Inspect NotCA", notCA: true})
	underNotCA := notCA.issue(t, issuance{cn: "host.example", leaf: true})
	noSigning := root.issue(t, issuance{cn: "Inspect NoSign", noSign: true})
	underNoSigning := noSigning.issue(t, issuance{cn: "host.example", leaf: true})
	for name, tc := range map[string]struct {
		chain []*testAuthority
		want  string
	}{
		"a middle intermediate missing":               {[]*testAuthority{leaf, three, one}, `"CN=Inspect Two" is missing. Paste it with the root.`},
		"an intermediate that is no CA":               {[]*testAuthority{underNotCA, notCA}, `"CN=Inspect NotCA" is no CA. Reissue it as a CA on the device.`},
		"an intermediate without certificate signing": {[]*testAuthority{underNoSigning, noSigning}, `"CN=Inspect NoSign" may not sign certificates. Reissue it with certificate signing.`},
		"a root that was not pasted":                  {[]*testAuthority{newTestRoot(t, "Other Root")}, `"CN=Other Root" is a root that was not pasted. Paste that root.`},
	} {
		if _, err := bundleFor(root, tc.chain...); err == nil || err.Error() != "Site CA: host.example: "+tc.want {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// TestSiteChainErrorsGroupTheHostsAndStayShort gives one line per fault, the hosts with the same fault on
// it, in at most 16 words and without semicolons.
func TestSiteChainErrorsGroupTheHostsAndStayShort(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	weakRoot := newTestRoot(t, "Weak Root")
	weak := weakRoot.issue(t, issuance{cn: "Inspect Weak", issuerTag: asn1.TagPrintableString, sig: x509.ECDSAWithSHA1})
	weakLeaf := weak.issue(t, issuance{cn: "host.example", leaf: true})
	a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{
		"factory.talos.dev": certs(c.leaf), "ghcr.io": certs(c.leaf), "tuf-repo-cdn.sigstore.dev": certs(weakLeaf, weak),
	})}
	s := &Settings{SiteCA: c.root.pem() + weakRoot.pem()}
	_, err := a.ResolveSite(t.Context(), s)
	want := []string{
		`Site CA: factory.talos.dev, ghcr.io: "CN=Inspect Intermediate" is missing. Paste it with the root.`,
		`Site CA: tuf-repo-cdn.sigstore.dev: "CN=Inspect Weak" is signed with ECDSA-SHA1, which nodes refuse. Reissue it with SHA-256.`,
	}
	faults, ok := errors.AsType[SiteChainErrors](err)
	if !ok || !slices.Equal(faults, want) {
		t.Fatalf("faults %q, want %q", err, want)
	}
	for _, line := range faults {
		_, text, _ := strings.Cut(strings.TrimPrefix(line, siteCALabel+" "), ": ")
		// The label is two words and the hosts one value, as web/WRITING.md counts a value.
		if words := 3 + len(strings.Fields(text)); words > 16 || strings.Contains(line, ";") {
			t.Errorf("%q has %d words", line, words)
		}
	}
}

// TestASiteRunWithoutAnswersKeepsTheRenamedRoots applies a Site CA whose hosts answer, then runs again
// while none does: the nodes keep the bundle that worked, so inheriting clusters see no change. A
// changed Site CA starts from nothing.
func TestASiteRunWithoutAnswersKeepsTheRenamedRoots(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a, ready := newAppliance(t)
	a.sitePublic = x509.NewCertPool()
	chain := certs(c.leaf, c.intermediate)
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"factory.talos.dev": chain, "ghcr.io": chain, "tuf-repo-cdn.sigstore.dev": chain})
	s := settings("pve1")
	s.SiteCA = c.root.pem()
	apply := func(s *Settings) {
		t.Helper()
		if err := a.Apply(s, "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
			t.Fatal(err)
		}
		if j := waitFor(t, a); j.State != StateSucceeded {
			t.Fatalf("job = %+v", j)
		}
		<-ready
	}
	apply(s)
	first := moduleSiteCAOf(t, a)
	if first == s.SiteCA || goTrusts(chain, first) != nil {
		t.Fatalf("the first run gave the nodes %q", first)
	}

	a.SiteChains = fakeChains(nil)
	again := settings("pve1")
	again.SiteCA = c.root.pem()
	apply(again)
	if got := moduleSiteCAOf(t, a); got != first {
		t.Errorf("a run where no host answered gave the nodes %q, want %q", got, first)
	}
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyUnattended(func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("automatic run = %+v", j)
	}
	<-ready
	if got := moduleSiteCAOf(t, a); got != first {
		t.Errorf("an automatic run where no host answered gave the nodes %q, want %q", got, first)
	}

	other := settings("pve1")
	other.SiteCA = newTestRoot(t, "Inspect Root").pem()
	apply(other)
	if got := moduleSiteCAOf(t, a); got != other.SiteCA {
		t.Errorf("a changed Site CA kept the copies of the last one: %q", got)
	}
}

// TestAnAutomaticRunDerivesTheBundle runs the saved settings unattended once the hosts answer, and
// gives the nodes the renamed root an earlier run could not find.
func TestAnAutomaticRunDerivesTheBundle(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a, ready := newAppliance(t)
	a.sitePublic = x509.NewCertPool()
	a.SiteChains = fakeChains(nil)
	s := settings("pve1")
	s.SiteCA = c.root.pem()
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	<-ready
	if got := moduleSiteCAOf(t, a); got != s.SiteCA {
		t.Fatalf("with no host answering the nodes got %q", got)
	}
	chain := certs(c.leaf, c.intermediate)
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": chain})
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyUnattended(func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	<-ready
	if got := moduleSiteCAOf(t, a); j.State != StateSucceeded || goTrusts(chain, got) != nil || a.SavedSettings().SiteBundle != got {
		t.Errorf("the automatic run (%s) gave the nodes %q", j.State, got)
	}
	if log := strings.Join(j.Log, "\n"); !strings.Contains(log, "not checked against factory.talos.dev, tuf-repo-cdn.sigstore.dev") {
		t.Errorf("the automatic run's log lacks the hosts that did not answer: %q", log)
	}
}

// moduleSiteCAOf is the Site CA the last run gave the module, and so the nodes and the controller.
func moduleSiteCAOf(t *testing.T, a *Appliance) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.workdir(), "terraform.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vars struct {
		Settings moduleSiteCA `json:"settings"`
	}
	if err = json.Unmarshal(raw, &vars); err != nil {
		t.Fatal(err)
	}
	return vars.Settings.SiteCA
}

// TestAProxyCAGetsTheRenamedRootsOfItsOwn treats the CA of a proxy that re-signs TLS as the Site CA
// is treated, with or without a Site CA beside it, and never as a public authority.
func TestAProxyCAGetsTheRenamedRootsOfItsOwn(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	chain := certs(c.leaf, c.intermediate)
	for name, siteCA := range map[string]string{"alone": "", "beside a Site CA": newTestRoot(t, "Site Root").pem()} {
		a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{"factory.talos.dev": chain, "ghcr.io": chain, "tuf-repo-cdn.sigstore.dev": chain})}
		s := &Settings{SiteCA: siteCA, Proxy: &Proxy{URL: "http://proxy.example:3128", CABundle: c.root.pem()}}
		if _, err := a.ResolveSite(t.Context(), s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.Proxy.Bundle == "" || s.SiteBundle != "" || goTrusts(chain, s.Proxy.trust()) != nil || goTrusts(chain, s.OutboundCA()) != nil {
			t.Errorf("%s: the proxy's bundle is %q, the Site CA's %q", name, s.Proxy.Bundle, s.SiteBundle)
		}
		if got := moduleProxy(s, "https://10.0.0.50:8006")[moduleCABundle]; got != s.Proxy.Bundle {
			t.Errorf("%s: the module's proxy trusts %q", name, got)
		}
	}
}

func TestSiteTrustForIsTheSavedBundleWhileTheSiteCAIsTheSame(t *testing.T) {
	t.Parallel()
	a := &Appliance{Dir: t.TempDir()}
	saved := &Settings{SiteCA: "pasted\n", SiteBundle: "pasted\nrenamed\n"}
	if err := os.MkdirAll(a.workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(a.workdir(), "settings.json"), saved); err != nil {
		t.Fatal(err)
	}
	if got := a.SiteTrustFor("pasted\n"); got != saved.SiteBundle {
		t.Errorf("the saved Site CA would give %q", got)
	}
	if got := a.SiteTrustFor("other\n"); got != "other\n" {
		t.Errorf("another Site CA would give %q", got)
	}
}

// TestSiteNoteReadsTheAppliancesOwnAddresses leaves Addresses unset, as serve does, so that the note
// reads the machine's interfaces.
func TestSiteNoteReadsTheAppliancesOwnAddresses(t *testing.T) {
	t.Parallel()
	a := &Appliance{SiteChains: fakeChains(nil)}
	own, err := a.addresses()
	if err != nil || len(own) == 0 {
		t.Skipf("this machine has no address to check from: %v", err)
	}
	s := settings("pve1")
	s.SiteCA = "x"
	s.NodeAddresses, s.NodeAddressesIPv6 = []string{"203.0.113.11/24"}, nil
	j := &Job{}
	a.noteSite(j, s, nil)
	want := "The Site CA was checked from the appliance at " + strings.Join(own, ", ") + "\nThe management nodes' network was not checked"
	if got := strings.Join(j.Log(), "\n"); got != want {
		t.Errorf("log %q, want %q", got, want)
	}
}

// connectProxy is a CONNECT proxy on the loopback that tunnels every request to target, and records
// what it was asked for.
func connectProxy(t *testing.T, target string) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.Host)
		mu.Unlock()
		upstream, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", target)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close() }()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(asked) }
}

func TestDialChainReadsWhatTheHostPresentsDirectlyAndThroughTheProxy(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{c.leaf.cert.Raw, c.intermediate.cert.Raw}, PrivateKey: c.leaf.key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	target := strings.TrimPrefix(server.URL, "https://")
	a := &Appliance{ProxmoxURL: "https://10.0.0.50:8006"}

	chain, err := a.DialChain(t.Context(), &Settings{}, target)
	if err != nil || len(chain) != 2 || !chain[0].Equal(c.leaf.cert) || !chain[1].Equal(c.intermediate.cert) {
		t.Errorf("directly: %d certificates, %v", len(chain), err)
	}
	proxyURL, asked := connectProxy(t, target)
	chain, err = a.DialChain(t.Context(), &Settings{Proxy: &Proxy{URL: proxyURL}}, "factory.talos.dev")
	if err != nil || len(chain) != 2 || !chain[0].Equal(c.leaf.cert) {
		t.Errorf("through the proxy: %d certificates, %v", len(chain), err)
	}
	if got := asked(); !slices.Equal(got, []string{"CONNECT factory.talos.dev:443"}) {
		t.Errorf("the proxy was asked %q", got)
	}
}

func TestDialChainGivesUpOnAHostThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	silent, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	go func() {
		for {
			conn, acceptErr := silent.Accept()
			if acceptErr != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	chain, err := (&Appliance{}).DialChain(ctx, &Settings{}, silent.Addr().String())
	if err == nil || len(chain) != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a silent host gave %d certificates and %v", len(chain), err)
	}
}

type fakeNodeLogs struct{ lines []string }

func (f fakeNodeLogs) Dmesg(context.Context) ([]string, error) { return f.lines, nil }

func (fakeNodeLogs) ServiceLog(context.Context, string) ([]string, error) { return nil, nil }

func (fakeNodeLogs) Close() error { return nil }

func TestAFailedApplyNamesTheNodeTheHostAndTheIssuerOfARejectedCertificate(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a, _ := newAppliance(t)
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"tuf-repo-cdn.sigstore.dev": certs(c.leaf, c.intermediate)})
	a.TalosLogs = func(_ context.Context, _ []byte, address string) (provisioner.TalosLogs, error) {
		if address != "10.0.0.11" {
			return fakeNodeLogs{lines: []string{"[talos] booted"}}, nil
		}
		return fakeNodeLogs{lines: []string{
			"[talos] booted",
			`[talos] controller failed {"controller": "security.TUFTrustedRootController", "error": "Get \"https://tuf-repo-cdn.sigstore.dev/root.json\": tls: failed to verify certificate: x509: certificate signed by unknown authority"}`,
		}}, nil
	}
	if err := a.Apply(settings("fail"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	want := "node 10.0.0.11 rejected the certificate tuf-repo-cdn.sigstore.dev presents, issued by CN=Inspect Intermediate. Check the Site CA. The run failed:"
	if j.State != StateFailed || !strings.Contains(j.Error, want) || strings.Contains(j.Error, "10.0.0.12") {
		t.Errorf("job error %q, want it to hold %q", j.Error, want)
	}
}
