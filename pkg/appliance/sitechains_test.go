package appliance

import (
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
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func newTestRoot(t *testing.T, cn string) *testAuthority {
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
	notAfter  time.Time
	sig       x509.SignatureAlgorithm
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
		IsCA: !o.leaf, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
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

// bundleFor is buildSiteBundle for one host's chain with the root pasted and no public authority.
func bundleFor(root *testAuthority, chain ...*testAuthority) (string, error) {
	return buildSiteBundle(root.pem(), certs(root), x509.NewCertPool(), []hostChain{{host: "host.example", chain: certs(chain...)}}, time.Now())
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
	if err == nil || !strings.Contains(err.Error(), "no chain from the certificate") || !strings.Contains(err.Error(), "CN=host.example") || !strings.Contains(err.Error(), "CN=Inspect Intermediate") {
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
