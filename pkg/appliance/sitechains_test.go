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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

var oidCommonName = asn1.ObjectIdentifier{2, 5, 4, 3}

// nameDER encodes a name of one common name as the given ASN.1 string type, which is how two
// encodings of one name differ for Go and not for OpenSSL. BMPString and UniversalString take two
// and four bytes a character, here for ASCII names.
func nameDER(t *testing.T, cn string, tag int) []byte {
	t.Helper()
	value := []byte(cn)
	if width := map[int]int{asn1.TagBMPString: bmpWidth, tagUniversalString: universalWidth}[tag]; width > 0 {
		value = nil
		for _, c := range []byte(cn) {
			value = append(value, make([]byte, width-1)...)
			value = append(value, c)
		}
	}
	der, err := asn1.Marshal(pkix.RDNSequence{{{Type: oidCommonName, Value: asn1.RawValue{Class: asn1.ClassUniversal, Tag: tag, Bytes: value}}}})
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
	// clientOnly makes a leaf for client authentication alone.
	clientOnly bool
	notAfter   time.Time
	sig        x509.SignatureAlgorithm
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
		if o.clientOnly {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
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
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
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
	if err == nil || err.Error() != `Site CA: host.example: Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.` {
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
	if want := []string{"factory.talos.dev", "registry.k8s.io", "tuf-repo-cdn.sigstore.dev"}; !slices.Equal(unchecked, want) {
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
	a := &Appliance{sitePublic: pool, SiteChains: fakeChains(map[string][]*x509.Certificate{"factory.talos.dev": chain, "ghcr.io": chain, "registry.k8s.io": chain, "tuf-repo-cdn.sigstore.dev": chain})}
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
			want: `"CN=Inspect Root" allows fewer intermediates than presented. Shorten the chain on the device.`,
		},
		"name constraints": {
			root: func(c *x509.Certificate) { c.PermittedDNSDomains = []string{"corp.example"} },
			want: `"CN=host.example" is outside its issuers' name constraints. Check them on the device.`,
		},
		"expired root": {
			root: func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Minute) },
			want: `"CN=Inspect Root" expired on`,
		},
	} {
		root := newTestRoot(t, "Inspect Root", tc.root)
		intermediate := root.issue(t, issuance{cn: "Inspect Intermediate", issuerTag: asn1.TagUTF8String})
		leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
		_, err := bundleFor(root, leaf, intermediate)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to hold %q", name, err, tc.want)
		} else if words := faultWords(err.Error()); words > 16 {
			t.Errorf("%s: %q has %d words", name, err, words)
		}
	}
}

// faultWords counts the words of a fault line as web/WRITING.md counts them: the label is two words
// and the hosts one value.
func faultWords(line string) int {
	_, text, _ := strings.Cut(strings.TrimPrefix(line, siteCALabel+" "), ": ")
	return 3 + len(strings.Fields(text))
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
	other := newTestRoot(t, "Other Root")
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
		"a middle intermediate missing":               {[]*testAuthority{leaf, three, one}, `Intermediate "CN=Inspect Two" is not presented. Have the device send it.`},
		"an intermediate that is no CA":               {[]*testAuthority{underNotCA, notCA}, `"CN=Inspect NotCA" is no CA. Reissue it as a CA on the device.`},
		"an intermediate without certificate signing": {[]*testAuthority{underNoSigning, noSigning}, `"CN=Inspect NoSign" may not sign certificates. Reissue it with the certificate signing key usage.`},
		"a root that was not pasted":                  {[]*testAuthority{other}, "Chain ends at \"CN=Other Root\" (SHA-256 " + Fingerprint(other.cert) + "). Add that root to the Site CA."},
	} {
		if _, err := bundleFor(root, tc.chain...); err == nil || err.Error() != "Site CA: host.example: "+tc.want {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// TestSiteChainErrorGroupTheHostsAndStayShort gives one line per fault, the hosts with the same fault on
// it, in at most 16 words and without semicolons.
func TestSiteChainErrorGroupTheHostsAndStayShort(t *testing.T) {
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
		`Site CA: factory.talos.dev, ghcr.io: Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`,
		`Site CA: tuf-repo-cdn.sigstore.dev: "CN=Inspect Weak" is signed with ECDSA-SHA1. Nodes refuse that. Reissue it with SHA-256.`,
	}
	faults, ok := errors.AsType[SiteChainError](err)
	if !ok || !slices.Equal(faults.Lines, want) {
		t.Fatalf("faults %q, want %q", err, want)
	}
	for _, line := range faults.Lines {
		if words := faultWords(line); words > 16 || strings.Contains(line, ";") {
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
	if log := strings.Join(j.Log, "\n"); !strings.Contains(log, "not checked against factory.talos.dev, registry.k8s.io, tuf-repo-cdn.sigstore.dev") {
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

// connectHandler is a CONNECT proxy that tunnels every request to target, and records what it was
// asked for.
func connectHandler(target string) (http.Handler, func() []string) {
	var mu sync.Mutex
	var asked []string
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}), func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(asked) }
}

// connectProxy is connectHandler on the loopback.
func connectProxy(t *testing.T, target string) (string, func() []string) {
	t.Helper()
	handler, asked := connectHandler(target)
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)
	return proxy.URL, asked
}

// tlsHost is a TLS server on the loopback that presents the chain, leaf first, and its address.
func tlsHost(t *testing.T, chain ...*testAuthority) string {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	raw := make([][]byte, 0, len(chain))
	for _, c := range chain {
		raw = append(raw, c.cert.Raw)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: raw, PrivateKey: chain[0].key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "https://")
}

func TestDialChainReadsWhatTheHostPresentsDirectlyAndThroughTheProxy(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	target := tlsHost(t, c.leaf, c.intermediate)
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

// TestDialChainVerifiesAnHTTPSProxyBeforeItSendsCredentials leaves the host unread through an https
// proxy whose certificate nothing trusts, before any CONNECT carries the password, and reads it
// through one the proxy's CA vouches for.
func TestDialChainVerifiesAnHTTPSProxyBeforeItSendsCredentials(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	target := tlsHost(t, c.leaf, c.intermediate)
	handler, asked := connectHandler(target)
	var mu sync.Mutex
	var auth []string
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	a := &Appliance{ProxmoxURL: "https://10.0.0.50:8006", sitePublic: x509.NewCertPool()}
	a.SiteChains = a.DialChain
	untrusted := &Settings{SiteCA: c.root.pem(), Proxy: &Proxy{URL: proxy.URL, User: "alice", Password: "s3cret"}}

	if chain, err := a.DialChain(t.Context(), untrusted, "factory.talos.dev"); err == nil || len(chain) != 0 {
		t.Errorf("an untrusted proxy gave %d certificates and %v", len(chain), err)
	}
	unchecked, err := a.ResolveSite(t.Context(), untrusted)
	if err != nil || !slices.Equal(unchecked, untrusted.siteHosts()) {
		t.Errorf("behind an untrusted proxy: unchecked %q, %v", unchecked, err)
	}
	mu.Lock()
	sent := slices.Clone(auth)
	mu.Unlock()
	if got := asked(); len(got) != 0 || len(sent) != 0 {
		t.Errorf("an untrusted proxy was asked %q with %q", got, sent)
	}

	proxyCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}))
	trusted := &Settings{Proxy: &Proxy{URL: proxy.URL, User: "alice", Password: "s3cret", CABundle: proxyCA}}
	chain, err := a.DialChain(t.Context(), trusted, "factory.talos.dev")
	if err != nil || len(chain) != 2 || !chain[0].Equal(c.leaf.cert) {
		t.Errorf("through a trusted proxy: %d certificates, %v", len(chain), err)
	}
	if got := asked(); !slices.Equal(got, []string{"CONNECT factory.talos.dev:443"}) {
		t.Errorf("a trusted proxy was asked %q", got)
	}
}

// TestSiteCheckLeavesAHostUncheckedWhenTheProxyRefusesTheTunnel judges no host on what a proxy that
// refuses CONNECT answers, its own certificate included, and saves.
func TestSiteCheckLeavesAHostUncheckedWhenTheProxyRefusesTheTunnel(t *testing.T) {
	t.Parallel()
	refuse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	plain := httptest.NewServer(refuse)
	t.Cleanup(plain.Close)
	inspecting := httptest.NewTLSServer(refuse)
	t.Cleanup(inspecting.Close)
	inspectingCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: inspecting.Certificate().Raw}))
	for name, proxy := range map[string]*Proxy{
		"http":  {URL: plain.URL},
		"https": {URL: inspecting.URL, CABundle: inspectingCA},
	} {
		a := &Appliance{ProxmoxURL: "https://10.0.0.50:8006", sitePublic: x509.NewCertPool()}
		a.SiteChains = a.DialChain
		s := &Settings{SiteCA: newInspected(t).root.pem(), Proxy: proxy}
		unchecked, err := a.ResolveSite(t.Context(), s)
		if err != nil || !slices.Equal(unchecked, s.siteHosts()) {
			t.Errorf("%s: unchecked %q, %v", name, unchecked, err)
		}
	}
}

// TestSiteBundleFoldsOnlyASCIICaseAsOpenSSLDoes copies a root for a name that differs in ASCII case
// and white space, and refuses one that differs in the case of another letter, as OpenSSL does.
func TestSiteBundleFoldsOnlyASCIICaseAsOpenSSLDoes(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "ÄRGER ROOT", func(c *x509.Certificate) { c.RawSubject = nameDER(t, "ÄRGER ROOT", asn1.TagUTF8String) })
	root.tag = asn1.TagUTF8String
	named := func(issuer string) []*testAuthority {
		alias := *root
		alias.cn = issuer
		intermediate := alias.issue(t, issuance{cn: "Inspect Intermediate"})
		return []*testAuthority{intermediate.issue(t, issuance{cn: "host.example", leaf: true}), intermediate}
	}
	ascii := named("  Ärger   root ")
	bundle, err := bundleFor(root, ascii...)
	if err != nil || goTrusts(certs(ascii...), bundle) != nil {
		t.Errorf("a name in other ASCII case and spacing: %v", err)
	}
	if _, err = bundleFor(root, named("äRGER ROOT")...); err == nil {
		t.Error("a name in other non-ASCII case was given a copy of the root")
	}
}

// TestSiteHostsAreTheOnesTheNodesReach reads a registry's https mirror in its place, never the
// registry, and the release mirror.
func TestSiteHostsAreTheOnesTheNodesReach(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var asked []string
	a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: func(_ context.Context, _ *Settings, host string) ([]*x509.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, host)
		return nil, errors.New("no answer")
	}}
	s := &Settings{
		SiteCA:          newInspected(t).root.pem(),
		RegistryMirrors: map[string][]string{"ghcr.io": {"https://mirror.example.org:5000", "http://plain.example.org"}},
		ReleaseMirror:   &ReleaseMirror{URL: "https://releases.example.org/github"},
	}
	if _, err := a.ResolveSite(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	slices.Sort(asked)
	want := []string{"factory.talos.dev", "mirror.example.org:5000", "registry.k8s.io", "releases.example.org", "tuf-repo-cdn.sigstore.dev"}
	if !slices.Equal(asked, want) {
		t.Errorf("read %q, want %q", asked, want)
	}
}

// TestAnUnattendedRunKeepsTheLastBundleThroughAChainFault goes on with the bundle of the last run
// and logs the fault once.
func TestAnUnattendedRunKeepsTheLastBundleThroughAChainFault(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a, ready := newAppliance(t)
	a.sitePublic = x509.NewCertPool()
	good := certs(c.leaf, c.intermediate)
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": good})
	s := settings("pve1")
	s.SiteCA = c.root.pem()
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	<-ready
	first := moduleSiteCAOf(t, a)
	if goTrusts(good, first) != nil {
		t.Fatalf("the first run gave the nodes %q", first)
	}

	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(c.leaf)})
	const fault = `Site CA: ghcr.io: Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyUnattended(func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != StateSucceeded {
		t.Fatalf("the automatic run %s: %q", j.State, j.Error)
	}
	<-ready
	if moduleSiteCAOf(t, a) != first || a.SavedSettings().SiteBundle != first {
		t.Error("the automatic run gave the nodes another bundle")
	}
	if n := strings.Count(strings.Join(j.Log, "\n"), fault); n != 1 {
		t.Errorf("the automatic run logged the fault %d times: %q", n, j.Log)
	}
}

func TestSiteBundleRefusesALeafForClientsOnly(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	leaf := c.intermediate.issue(t, issuance{cn: "host.example", leaf: true, clientOnly: true})
	const want = `Site CA: host.example: "CN=host.example" is not for servers. Reissue it for server authentication.`
	if _, err := bundleFor(c.root, leaf, c.intermediate); err == nil || err.Error() != want {
		t.Errorf("a leaf for clients only gave %v, want %q", err, want)
	}
}

// TestAProxyCAFaultNamesTheProxyCertificate labels a fault in a chain of the proxy's CA with the
// proxy's field, also where the chain names neither root.
func TestAProxyCAFaultNamesTheProxyCertificate(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	old := c.root.issue(t, issuance{cn: "Inspect Old", issuerTag: asn1.TagUTF8String, notAfter: time.Now().Add(-time.Minute)})
	for name, chain := range map[string][]*x509.Certificate{
		"an expired intermediate": certs(old.issue(t, issuance{cn: "host.example", leaf: true}), old),
		"a missing intermediate":  certs(c.leaf),
	} {
		a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": chain})}
		s := &Settings{SiteCA: newTestRoot(t, "Site Root").pem(), Proxy: &Proxy{URL: "http://proxy.example:3128", CABundle: c.root.pem()}}
		_, err := a.ResolveSite(t.Context(), s)
		if err == nil || !strings.HasPrefix(err.Error(), "Proxy certificate: ghcr.io: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestSiteBundleNamesARootTheDeviceMadeAgain names the intermediate a new root of the same name
// signs, and the new root where the device presents it.
func TestSiteBundleNamesARootTheDeviceMadeAgain(t *testing.T) {
	t.Parallel()
	pasted := newTestRoot(t, "Inspect Root")
	c := newInspected(t)
	for name, tc := range map[string]struct {
		chain []*testAuthority
		want  string
	}{
		"without the new root": {[]*testAuthority{c.leaf, c.intermediate}, `"CN=Inspect Intermediate" is signed by a new "CN=Inspect Root". Paste the new root.`},
		"with the new root":    {[]*testAuthority{c.leaf, c.intermediate, c.root}, "Chain ends at \"CN=Inspect Root\" (SHA-256 " + Fingerprint(c.root.cert) + "). Add that root to the Site CA."},
	} {
		if _, err := bundleFor(pasted, tc.chain...); err == nil || err.Error() != "Site CA: host.example: "+tc.want {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
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
	want := "Node 10.0.0.11 refused tuf-repo-cdn.sigstore.dev's certificate. Add \"CN=Inspect Root\" to the Site CA. The run failed:"
	if j.State != StateFailed || !strings.HasPrefix(j.Error, want) || strings.Contains(j.Error, "10.0.0.12") {
		t.Errorf("job error %q, want it to hold %q", j.Error, want)
	}
}

// TestAFailedApplyNamesTheRootTheRejectedChainEndsAt names the root the host presents at the end of
// the chain the node refused, with its fingerprint, and not the first issuer.
func TestAFailedApplyNamesTheRootTheRejectedChainEndsAt(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a, _ := newAppliance(t)
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"tuf-repo-cdn.sigstore.dev": certs(c.leaf, c.intermediate, c.root)})
	a.TalosLogs = func(_ context.Context, _ []byte, address string) (provisioner.TalosLogs, error) {
		if address != "10.0.0.11" {
			return fakeNodeLogs{lines: []string{"[talos] booted"}}, nil
		}
		return fakeNodeLogs{lines: []string{
			`[talos] controller failed {"controller": "security.TUFTrustedRootController", "error": "Get \"https://tuf-repo-cdn.sigstore.dev/root.json\": tls: failed to verify certificate: x509: certificate signed by unknown authority"}`,
		}}, nil
	}
	if err := a.Apply(settings("fail"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	want := "Node 10.0.0.11 refused tuf-repo-cdn.sigstore.dev's certificate. Add its root \"CN=Inspect Root\" (SHA-256 " + Fingerprint(c.root.cert) + ") to the Site CA. The run failed:"
	if j.State != StateFailed || !strings.HasPrefix(j.Error, want) {
		t.Errorf("job error %q, want it to start with %q", j.Error, want)
	}
}

// TestSiteBundleKeepsANumericStringNameApartAsOpenSSLDoes gives no copy of a root in PrintableString
// for an issuer named in NumericString, which OpenSSL compares by type and bytes.
func TestSiteBundleKeepsANumericStringNameApartAsOpenSSLDoes(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "12345")
	intermediate := root.issue(t, issuance{cn: "Inspect Intermediate", issuerTag: asn1.TagNumericString})
	leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
	if bundle, err := bundleFor(root, leaf, intermediate); err == nil {
		t.Errorf("a NumericString issuer was given a copy of the root: %q", bundle)
	}
}

// TestSiteBundleCopiesARootNamedInBMPOrUniversalString accepts an issuer that names a PrintableString
// root in the same characters in BMPString or UniversalString, as OpenSSL does.
func TestSiteBundleCopiesARootNamedInBMPOrUniversalString(t *testing.T) {
	t.Parallel()
	for name, tag := range map[string]int{"BMPString": asn1.TagBMPString, "UniversalString": tagUniversalString} {
		root := newTestRoot(t, "Inspect Root")
		intermediate := root.issue(t, issuance{cn: "Inspect Intermediate", issuerTag: tag})
		leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
		bundle, err := bundleFor(root, leaf, intermediate)
		if err != nil || goTrusts(certs(leaf, intermediate), bundle) != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestSiteFaultNamesAnIssuerInUniversalString names a missing intermediate whose name is in
// UniversalString, which Go's own text of the name leaves out.
func TestSiteFaultNamesAnIssuerInUniversalString(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t, "Inspect Root")
	intermediate := root.issue(t, issuance{cn: "Inspect Intermediate", tag: tagUniversalString})
	leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
	const want = `Site CA: host.example: Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`
	if _, err := bundleFor(root, leaf); err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}

// TestASaveThatKeepsTheSiteCAGoesOnPastAChainFault changes another setting while one host presents
// an expired intermediate: the run keeps the last bundle and shows the fault as a warning, as the
// live check does.
func TestASaveThatKeepsTheSiteCAGoesOnPastAChainFault(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	old := c.root.issue(t, issuance{cn: "Inspect Old", issuerTag: asn1.TagUTF8String, notAfter: time.Now().Add(-time.Minute)})
	a, ready := newAppliance(t)
	a.sitePublic = x509.NewCertPool()
	good := certs(c.leaf, c.intermediate)
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": good})
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
	first := moduleSiteCAOf(t, a)
	if goTrusts(good, first) != nil {
		t.Fatalf("the first run gave the nodes %q", first)
	}

	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": good, "registry.k8s.io": certs(old.issue(t, issuance{cn: "host.example", leaf: true}), old)})
	again := settings("pve1")
	again.SiteCA, again.TimeZone = c.root.pem(), "Europe/Zurich"
	fault := `Site CA: registry.k8s.io: "CN=Inspect Old" expired on ` + old.cert.NotAfter.UTC().Format(time.DateOnly) + ". Renew it on the device."
	if got := a.CheckSite(t.Context(), again); len(got.Errors) != 0 || !slices.Equal(got.Warnings, []string{fault}) {
		t.Errorf("the live check gave errors %q and warnings %q, want the warning %q", got.Errors, got.Warnings, fault)
	}
	j := apply(again)
	saved := a.SavedSettings()
	if j.State != StateSucceeded || saved.TimeZone != "Europe/Zurich" || saved.SiteBundle != first || moduleSiteCAOf(t, a) != first {
		t.Errorf("the run (%s) saved the time zone %q and gave the nodes another bundle", j.State, saved.TimeZone)
	}
	if n := strings.Count(strings.Join(j.Log, "\n"), fault); n != 1 {
		t.Errorf("the run logged the fault %d times: %q", n, j.Log)
	}
}

// TestASaveThatChangesTheSiteCARefusesAChainFault refuses a chain fault on the first save of a Site
// CA and on a save that changes it.
func TestASaveThatChangesTheSiteCARefusesAChainFault(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a, ready := newAppliance(t)
	a.sitePublic = x509.NewCertPool()
	missing := fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(c.leaf)})
	const fault = `Site CA: ghcr.io: Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`
	a.SiteChains = missing
	first := settings("pve1")
	first.SiteCA = c.root.pem()
	if err := a.Apply(first, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err == nil || err.Error() != fault {
		t.Errorf("a first save gave %v", err)
	}

	a.SiteChains = fakeChains(nil)
	other := settings("pve1")
	other.SiteCA = newTestRoot(t, "Other Root").pem()
	if err := a.Apply(other, "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	<-ready
	a.SiteChains = missing
	changed := settings("pve1")
	changed.SiteCA = c.root.pem()
	if got := a.CheckSite(t.Context(), changed); !slices.Equal(got.Errors, []string{fault}) || len(got.Warnings) != 0 {
		t.Errorf("the live check gave errors %q and warnings %q", got.Errors, got.Warnings)
	}
	if err := a.Apply(changed, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err == nil || err.Error() != fault {
		t.Errorf("a changed Site CA gave %v", err)
	}
}

// TestSiteCheckReadsThroughAnHTTPSProxyThatNeedsTheKeptCopy reads the hosts through an https proxy
// whose own chain names the pasted root in other bytes, once the saved bundle holds the copy.
func TestSiteCheckReadsThroughAnHTTPSProxyThatNeedsTheKeptCopy(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	handler, asked := connectHandler(tlsHost(t, c.leaf, c.intermediate))
	proxy := httptest.NewUnstartedServer(handler)
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{c.leaf.cert.Raw, c.intermediate.cert.Raw}, PrivateKey: c.leaf.key}}}
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	renamed, err := withSubject(c.root.cert, c.intermediate.cert.RawIssuer)
	if err != nil {
		t.Fatal(err)
	}
	a := &Appliance{Dir: t.TempDir(), ProxmoxURL: "https://10.0.0.50:8006", sitePublic: x509.NewCertPool()}
	a.SiteChains = a.DialChain
	saved := &Settings{SiteCA: c.root.pem(), SiteBundle: c.root.pem() + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: renamed.Raw}))}
	if err = os.MkdirAll(a.workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = writeJSONFile(filepath.Join(a.workdir(), "settings.json"), saved); err != nil {
		t.Fatal(err)
	}
	s := &Settings{SiteCA: c.root.pem(), Proxy: &Proxy{URL: proxy.URL}}
	unchecked, err := a.ResolveSite(t.Context(), s)
	if err != nil || len(unchecked) != 0 || s.SiteBundle != saved.SiteBundle {
		t.Errorf("unchecked %q, %v, bundle %q", unchecked, err, s.SiteBundle)
	}
	if got := asked(); len(got) != len(s.siteHosts()) {
		t.Errorf("the proxy was asked %q", got)
	}
}

// presentedRootFault is the line a chain that ends at a root the Site CA lacks gets.
func presentedRootFault(hosts string, root *testAuthority) string {
	return "Site CA: " + hosts + ": Chain ends at \"CN=" + root.cn + "\" (SHA-256 " + Fingerprint(root.cert) + "). Add that root to the Site CA."
}

// TestSiteCheckNamesThePresentedRootTheSiteCALacksAndPassesOnceItIsAdded judges a chain that ends at
// a self-signed root the host presents, against a Site CA of another root. The appliance's own trust
// store is no part of it. The fault hands the root over as PEM, and a Site CA that holds it passes.
func TestSiteCheckNamesThePresentedRootTheSiteCALacksAndPassesOnceItIsAdded(t *testing.T) {
	t.Parallel()
	inspection := newTestRoot(t, "Internal-R1")
	intermediate := inspection.issue(t, issuance{cn: "Inspect Intermediate", issuerTag: asn1.TagPrintableString})
	leaf := intermediate.issue(t, issuance{cn: "host.example", leaf: true})
	chain := certs(leaf, intermediate, inspection)
	a := &Appliance{SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": chain, "registry.k8s.io": chain})}
	other := newTestRoot(t, "Internal-R2")

	got := a.CheckSite(t.Context(), &Settings{SiteCA: other.pem()})
	if want := []string{presentedRootFault("ghcr.io, registry.k8s.io", inspection)}; !slices.Equal(got.Errors, want) {
		t.Fatalf("errors %q, want %q", got.Errors, want)
	}
	if want := []MissingRoot{{Name: "Internal-R1", PEM: inspection.pem()}}; !slices.Equal(got.MissingRoots, want) {
		t.Fatalf("missing roots %q, want %q", got.MissingRoots, want)
	}
	if words := faultWords(got.Errors[0]); words > 16 {
		t.Errorf("%q has %d words", got.Errors[0], words)
	}

	for name, siteCA := range map[string]string{
		"the offered root appended": WithRoots(other.pem(), got.MissingRoots),
		"the root alone":            inspection.pem(),
		"several roots, one right":  other.pem() + newTestRoot(t, "Internal-R3").pem() + inspection.pem(),
	} {
		if again := a.CheckSite(t.Context(), &Settings{SiteCA: siteCA}); len(again.Errors) != 0 || len(again.Warnings) != 0 || len(again.MissingRoots) != 0 {
			t.Errorf("%s: %+v", name, again)
		}
	}
}

// TestWithRootsAppendsEachRootOnce adds the roots a check hands over to a Site CA, none it holds and
// none twice.
func TestWithRootsAppendsEachRootOnce(t *testing.T) {
	t.Parallel()
	a, b := newTestRoot(t, "Internal-R1"), newTestRoot(t, "Internal-R2")
	ra, rb := MissingRoot{Name: "Internal-R1", PEM: a.pem()}, MissingRoot{Name: "Internal-R2", PEM: b.pem()}
	for name, tc := range map[string]struct {
		siteCA string
		roots  []MissingRoot
		want   string
	}{
		"an empty Site CA":    {"", []MissingRoot{ra}, a.pem()},
		"one held, one new":   {a.pem(), []MissingRoot{ra, rb}, a.pem() + b.pem()},
		"the same root twice": {"", []MissingRoot{rb, rb}, b.pem()},
		"no final line break": {strings.TrimSpace(a.pem()), []MissingRoot{rb}, a.pem() + b.pem()},
	} {
		if got := WithRoots(tc.siteCA, tc.roots); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// TestSiteCheckOffersNoRootTheHostDidNotPresent names the issuer of the topmost certificate and has
// no PEM to hand over.
func TestSiteCheckOffersNoRootTheHostDidNotPresent(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a := &Appliance{SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(c.leaf, c.intermediate)})}
	got := a.CheckSite(t.Context(), &Settings{SiteCA: newTestRoot(t, "Internal-R2").pem()})
	if want := []string{`Site CA: ghcr.io: Issuer "CN=Inspect Root" is not presented. Add that root to the Site CA.`}; !slices.Equal(got.Errors, want) || len(got.MissingRoots) != 0 {
		t.Errorf("got %+v, want errors %q and no roots", got, want)
	}
}

// TestTheNodesPublicRootsAreMozillasAndNotTheContainers holds a public root and none of the roots
// a test generates, so that the container's own store never decides a chain is public.
func TestTheNodesPublicRootsAreMozillasAndNotTheContainers(t *testing.T) {
	t.Parallel()
	a := &Appliance{}
	pool := a.publicRoots()
	subjects := pool.Subjects() //nolint:staticcheck // reads names only, for a pool built from roots
	found := slices.ContainsFunc(subjects, func(s []byte) bool { return bytes.Contains(s, []byte("ISRG Root X1")) })
	if !found {
		t.Errorf("the nodes' roots lack ISRG Root X1 among %d roots", len(subjects))
	}
	private := newTestRoot(t, "Internal-R1")
	leaf := private.issue(t, issuance{cn: "host.example", leaf: true})
	if verifyChain(certs(leaf), pool, time.Now()) == nil {
		t.Error("a chain of a generated root verifies against the nodes' roots")
	}
}

// TestAChainSignedByARootOnlyTheContainerTrustsIsAFault reads the chain of a generated root that the
// process's system store holds, as the appliance container's does after the installer added it, and
// judges it against a Site CA of another root. The system store is set through SSL_CERT_FILE, which
// only Linux reads, so the check runs in a child process there.
func TestAChainSignedByARootOnlyTheContainerTrustsIsAFault(t *testing.T) {
	t.Parallel()
	if dir := os.Getenv("KUBEHENGE_TEST_CHAIN_DIR"); dir != "" {
		chain, err := parseCertificates(readTestFile(t, filepath.Join(dir, "chain.pem")))
		if err != nil {
			t.Fatal(err)
		}
		system, err := x509.SystemCertPool()
		if err != nil || verifyChain(chain, system, time.Now()) != nil {
			t.Fatalf("the system store does not trust the chain: %v", err)
		}
		a := &Appliance{SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": chain})}
		got := a.CheckSite(t.Context(), &Settings{SiteCA: readTestFile(t, filepath.Join(dir, "site.pem"))})
		want := "Site CA: ghcr.io: Chain ends at \"CN=Internal-R1\" (SHA-256 " + Fingerprint(chain[len(chain)-1]) + "). Add that root to the Site CA."
		if !slices.Equal(got.Errors, []string{want}) {
			t.Fatalf("errors %q, want %q", got.Errors, want)
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("the system store is set through SSL_CERT_FILE, which only Linux reads")
	}
	inspection := newTestRoot(t, "Internal-R1")
	leaf := inspection.issue(t, issuance{cn: "host.example", leaf: true})
	dir := t.TempDir()
	for name, content := range map[string]string{"chain.pem": leaf.pem() + inspection.pem(), "root.pem": inspection.pem(), "site.pem": newTestRoot(t, "Internal-R2").pem()} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], //nolint:gosec // this test binary, run again for one test
		"-test.run=^TestAChainSignedByARootOnlyTheContainerTrustsIsAFault$")
	cmd.Env = append(os.Environ(), "KUBEHENGE_TEST_CHAIN_DIR="+dir, "SSL_CERT_FILE="+filepath.Join(dir, "root.pem"), "SSL_CERT_DIR="+dir+"/none")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("the child run failed: %v\n%s", err, out)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a file the test wrote
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestSiteCheckTellsAMissingIntermediateFromAMissingRoot asks the device for an intermediate where
// the Site CA may well hold the root: the host sent no intermediate, or one the path does not reach.
// A chain whose every presented certificate is on the path lacks its root.
func TestSiteCheckTellsAMissingIntermediateFromAMissingRoot(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	stray := newTestRoot(t, "Stray Root").issue(t, issuance{cn: "Stray Intermediate"})
	for name, tc := range map[string]struct {
		siteCA string
		chain  []*x509.Certificate
		want   string
	}{
		"the root pasted, no intermediate sent": {c.root.pem(), certs(c.leaf), `Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`},
		"the root pasted, another one sent":     {c.root.pem(), certs(c.leaf, stray), `Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`},
		"another root pasted, the chain whole":  {newTestRoot(t, "Internal-R2").pem(), certs(c.leaf, c.intermediate), `Issuer "CN=Inspect Root" is not presented. Add that root to the Site CA.`},
	} {
		a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": tc.chain})}
		got := a.CheckSite(t.Context(), &Settings{SiteCA: tc.siteCA})
		if want := []string{"Site CA: ghcr.io: " + tc.want}; !slices.Equal(got.Errors, want) || len(got.MissingRoots) != 0 {
			t.Errorf("%s: %+v, want %q", name, got, want)
		}
	}
}

// TestAProxyChainEndingAtARootItsCALacksNamesTheProxysCA names the proxy's field and hands no root
// over, since the button adds to the Site CA.
func TestAProxyChainEndingAtARootItsCALacksNamesTheProxysCA(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	stray := newTestRoot(t, "Stray Root")
	leaf := stray.issue(t, issuance{cn: "host.example", leaf: true})
	a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(leaf, stray)})}
	got := a.CheckSite(t.Context(), &Settings{Proxy: &Proxy{URL: "http://proxy.example:3128", CABundle: c.root.pem()}})
	want := "Proxy certificate: ghcr.io: Chain ends at \"CN=Stray Root\" (SHA-256 " + Fingerprint(stray.cert) + "). Add that root to the proxy's CA."
	if !slices.Equal(got.Errors, []string{want}) || len(got.MissingRoots) != 0 {
		t.Errorf("got %+v, want the error %q and no roots", got, want)
	}
}

// TestSiteCheckHandsOverTheMissingRootWithAWarning offers the root a host presents also where the
// save would go on past the fault with the last run's certificates.
func TestSiteCheckHandsOverTheMissingRootWithAWarning(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	a, ready := newAppliance(t)
	a.sitePublic = x509.NewCertPool()
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(c.leaf, c.intermediate)})
	s := settings("pve1")
	s.SiteCA = c.root.pem()
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("the first run %s: %s", j.State, j.Error)
	}
	<-ready
	other := newTestRoot(t, "Internal-R1")
	leaf := other.issue(t, issuance{cn: "host.example", leaf: true})
	a.SiteChains = fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(c.leaf, c.intermediate), "registry.k8s.io": certs(leaf, other)})
	again := settings("pve1")
	again.SiteCA = c.root.pem()
	got := a.CheckSite(t.Context(), again)
	if len(got.Errors) != 0 || !slices.Equal(got.Warnings, []string{presentedRootFault("registry.k8s.io", other)}) || !slices.Equal(got.MissingRoots, []MissingRoot{{Name: "Internal-R1", PEM: other.pem()}}) {
		t.Errorf("got %+v, want the warning and the root", got)
	}
}

// lineWords counts the words of a message as web/WRITING.md does, a quoted value as one.
func lineWords(line string) int {
	return len(strings.Fields(regexp.MustCompile(`"[^"]*"`).ReplaceAllString(line, "value")))
}

// TestAFailedRunLineFollowsTheNodesLog builds the line from what the node logged first, and from
// the chain the host presents now only where the node named no authority and no public root signs
// that chain. No line tells the person to add a public root.
func TestAFailedRunLineFollowsTheNodesLog(t *testing.T) {
	t.Parallel()
	c := newInspected(t)
	const host = "tuf-repo-cdn.sigstore.dev"
	logLine := func(x509Error string) string {
		return `[talos] controller failed {"controller": "security.TUFTrustedRootController", "error": "Get \"https://` + host + `/root.json\": tls: failed to verify certificate: x509: ` + x509Error + `"}`
	}
	candidate := func(name string) string {
		return logLine(`certificate signed by unknown authority (possibly because of \"x509: invalid signature\" while trying to verify candidate authority certificate \"` + name + `\")`)
	}
	unknown := logLine("certificate signed by unknown authority")
	public := newTestRoot(t, "Public Root")
	publicChain := certs(public.issue(t, issuance{cn: "host.example", leaf: true}), public)
	for name, tc := range map[string]struct {
		line   string
		chain  []*x509.Certificate
		public *x509.CertPool
		want   string
	}{
		"a candidate the Site CA holds, the live chain public": {
			candidate("Corp Inspection CA"), publicChain, poolOf(certs(public)),
			`Node 10.0.0.11 refused ` + host + `'s certificate, signed by a new "Corp Inspection CA". Paste the new root.`,
		},
		"a candidate that is a public root": {
			candidate("ISRG Root X1"), certs(c.leaf, c.intermediate, c.root), nil,
			`Node 10.0.0.11 refused ` + host + `'s certificate: certificate signed by unknown authority.`,
		},
		"another reason": {
			logLine("certificate has expired or is not yet valid: current time 2026-11-26T10:00:00Z is after 2026-11-25T00:00:00Z"), certs(c.leaf, c.intermediate, c.root), nil,
			`Node 10.0.0.11 refused ` + host + `'s certificate: certificate has expired or is not yet valid.`,
		},
		"no candidate, a live chain a public root signs": {
			unknown, publicChain, poolOf(certs(public)),
			`Node 10.0.0.11 refused ` + host + `'s certificate: certificate signed by unknown authority. Check the Site CA.`,
		},
		"no candidate, no live chain": {
			unknown, nil, nil,
			`Node 10.0.0.11 refused ` + host + `'s certificate: certificate signed by unknown authority. Check the Site CA.`,
		},
		"no candidate, a presented root": {
			unknown, certs(c.leaf, c.intermediate, c.root), nil,
			`Node 10.0.0.11 refused ` + host + `'s certificate. Add its root "CN=Inspect Root" (SHA-256 ` + Fingerprint(c.root.cert) + `) to the Site CA.`,
		},
		"no candidate, the root not presented": {
			unknown, certs(c.leaf, c.intermediate), nil,
			`Node 10.0.0.11 refused ` + host + `'s certificate. Add "CN=Inspect Root" to the Site CA.`,
		},
	} {
		a := &Appliance{sitePublic: tc.public}
		if tc.chain != nil {
			a.SiteChains = fakeChains(map[string][]*x509.Certificate{host: tc.chain})
		}
		r, found := rejectedCertificate([]string{"[talos] booted", tc.line})
		if !found {
			t.Fatalf("%s: no rejection found", name)
		}
		got := a.refusalLine(t.Context(), &Settings{}, "10.0.0.11", r)
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
		if words := lineWords(got); words > 16 || strings.Contains(got, ";") {
			t.Errorf("%s: %q has %d words", name, got, words)
		}
	}
}
