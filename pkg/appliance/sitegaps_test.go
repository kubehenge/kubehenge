package appliance

import (
	"cmp"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSiteCheckNamesWhatAChainLacksByWhatIsMissing names the first issuer that neither the host nor
// the Site CA holds. It asks the device for it only where a certificate the host presents above the
// gap leads to a pasted root.
func TestSiteCheckNamesWhatAChainLacksByWhatIsMissing(t *testing.T) {
	t.Parallel()
	other := newTestRoot(t, "Internal-R2")
	device := newTestRoot(t, "Device Root")
	direct := device.issue(t, issuance{cn: "host.example", leaf: true})
	corp := newTestRoot(t, "Corp Root")
	one := corp.issue(t, issuance{cn: "Corp Issuing 1"})
	two := one.issue(t, issuance{cn: "Corp Issuing 2"})
	underTwo := two.issue(t, issuance{cn: "host.example", leaf: true})
	inspect := corp.issue(t, issuance{cn: "Corp Inspect"})
	underInspect := inspect.issue(t, issuance{cn: "host.example", leaf: true})
	for name, tc := range map[string]struct {
		siteCA string
		chain  []*x509.Certificate
		want   string
		roots  []MissingRoot
	}{
		"the device's root signs the leaf, sent alone": {other.pem(), certs(direct), `Add "CN=Device Root" to the Site CA.`, nil},
		"the device's root signs the leaf, root sent": {
			other.pem(), certs(direct, device),
			"Chain ends at \"CN=Device Root\" (SHA-256 " + Fingerprint(device.cert) + "). Add that root to the Site CA.",
			[]MissingRoot{{Name: "Device Root", PEM: device.pem()}},
		},
		"the device's root pasted":                        {device.pem(), certs(direct), "", nil},
		"the root pasted, the upper intermediate missing": {corp.pem(), certs(underTwo, two), `Add "CN=Corp Issuing 1" to the Site CA.`, nil},
		"the upper intermediate added to the Site CA":     {corp.pem() + one.pem(), certs(underTwo, two), "", nil},
		"the root pasted, the leaf sent alone":            {corp.pem(), certs(underInspect), `Add "CN=Corp Inspect" to the Site CA.`, nil},
		"another root pasted, the leaf sent alone":        {other.pem(), certs(underInspect), `Add "CN=Corp Inspect" to the Site CA.`, nil},
		"the root pasted and sent, an intermediate left out": {
			corp.pem(), certs(underTwo, corp), `Have the device send "CN=Corp Issuing 2", or add it to the Site CA.`, nil,
		},
		"the root pasted, the lower intermediate left out": {
			corp.pem(), certs(underTwo, one), `Have the device send "CN=Corp Issuing 2", or add it to the Site CA.`, nil,
		},
	} {
		a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": tc.chain})}
		got := a.CheckSite(t.Context(), &Settings{SiteCA: tc.siteCA})
		var want []string
		if tc.want != "" {
			want = []string{"Site CA: ghcr.io: " + tc.want}
		}
		if !slices.Equal(got.Errors, want) || !slices.Equal(got.MissingRoots, tc.roots) {
			t.Errorf("%s: %+v, want %q and roots %v", name, got, want, tc.roots)
		}
		for _, line := range got.Errors {
			if _, fault, _ := strings.Cut(strings.TrimPrefix(line, siteCALabel+" "), ": "); lineWords(fault) > 16 {
				t.Errorf("%s: %q has more than 16 words", name, line)
			}
		}
	}
}

// TestTheIncidentsChainIsRefusedAndNamed judges a chain of an inspection CA under two intermediates
// and a presented internal root, with another internal root as the Site CA.
func TestTheIncidentsChainIsRefusedAndNamed(t *testing.T) {
	t.Parallel()
	r1 := newTestRoot(t, "Internal-R1")
	i1 := r1.issue(t, issuance{cn: "Internal Issuing 1"})
	i2 := i1.issue(t, issuance{cn: "Internal Issuing 2"})
	inspection := i2.issue(t, issuance{cn: "Inspection CA"})
	leaf := inspection.issue(t, issuance{cn: "host.example", leaf: true})
	r2 := newTestRoot(t, "Internal-R2")
	whole := certs(leaf, inspection, i2, i1, r1)
	check := func(siteCA string, chain []*x509.Certificate) SiteCheck {
		a := &Appliance{SiteChains: fakeChains(map[string][]*x509.Certificate{"registry.k8s.io": chain, "ghcr.io": chain})}
		return a.CheckSite(t.Context(), &Settings{SiteCA: siteCA})
	}
	got := check(r2.pem(), whole)
	want := "Site CA: ghcr.io, registry.k8s.io: Chain ends at \"CN=Internal-R1\" (SHA-256 " + Fingerprint(r1.cert) + "). Add that root to the Site CA."
	if !slices.Equal(got.Errors, []string{want}) || !slices.Equal(got.MissingRoots, []MissingRoot{{Name: "Internal-R1", PEM: r1.pem()}}) {
		t.Errorf("the chain with its root: %+v, want %q", got, want)
	}
	for name, siteCA := range map[string]string{"the offered root added": WithRoots(r2.pem(), got.MissingRoots), "both roots pasted": r2.pem() + r1.pem()} {
		if again := check(siteCA, whole); len(again.Errors) != 0 || len(again.MissingRoots) != 0 {
			t.Errorf("%s: %+v", name, again)
		}
	}
	if got := check(r2.pem(), certs(leaf, inspection, i2, i1)); !slices.Equal(got.Errors, []string{`Site CA: ghcr.io, registry.k8s.io: Add "CN=Internal-R1" to the Site CA.`}) {
		t.Errorf("the chain without its root: %+v", got)
	}
}

// TestAPublicRootIsNeverOfferedForAChainTheNodesRefuseForAnotherReason says why the nodes refuse a
// chain that ends at one of their public roots, and hands no root over.
func TestAPublicRootIsNeverOfferedForAChainTheNodesRefuseForAnotherReason(t *testing.T) {
	t.Parallel()
	public := newTestRoot(t, "Public Test Root")
	leaf := public.issue(t, issuance{cn: "host.example", leaf: true, clientOnly: true})
	a := &Appliance{sitePublic: poolOf(certs(public)), SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": certs(leaf, public)})}
	got := a.CheckSite(t.Context(), &Settings{SiteCA: newTestRoot(t, "Internal-R2").pem()})
	if len(got.Errors) != 1 || strings.Contains(got.Errors[0], "Add") || !strings.Contains(got.Errors[0], "is not for servers") || len(got.MissingRoots) != 0 {
		t.Errorf("got %+v, want the key usage named and no root", got)
	}
}

// TestTwoMissingRootsOfOneNameGetTheirOwnNames tells apart by fingerprint the roots of one name that
// two hosts end at, so that the buttons and the CLI name each once.
func TestTwoMissingRootsOfOneNameGetTheirOwnNames(t *testing.T) {
	t.Parallel()
	first, second := newTestRoot(t, "Corp Root CA"), newTestRoot(t, "Corp Root CA")
	a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{
		"ghcr.io":         certs(first.issue(t, issuance{cn: "host.example", leaf: true}), first),
		"registry.k8s.io": certs(second.issue(t, issuance{cn: "host.example", leaf: true}), second),
	})}
	got := a.CheckSite(t.Context(), &Settings{SiteCA: newTestRoot(t, "Internal-R2").pem()})
	if len(got.MissingRoots) != 2 || got.MissingRoots[0].Name == got.MissingRoots[1].Name ||
		!strings.HasPrefix(got.MissingRoots[0].Name, "Corp Root CA (") || !strings.Contains(got.MissingRoots[1].Name, Fingerprint(second.cert)[:11]) &&
		!strings.Contains(got.MissingRoots[0].Name, Fingerprint(second.cert)[:11]) {
		t.Errorf("roots %+v, want two names told apart by fingerprint", got.MissingRoots)
	}
}

// TestAFailedRunLineNeverAsksToAddARootTheSiteCAHolds says to check the Site CA where the node named
// no authority and the chain the host presents now verifies against the saved Site CA, or ends at a
// root it holds. A presented public root gets no Site CA advice.
func TestAFailedRunLineNeverAsksToAddARootTheSiteCAHolds(t *testing.T) {
	t.Parallel()
	const host = "tuf-repo-cdn.sigstore.dev"
	corp := newTestRoot(t, "Corp Root")
	inspect := corp.issue(t, issuance{cn: "Corp Inspect"})
	leaf := inspect.issue(t, issuance{cn: "host.example", leaf: true})
	remade := newTestRoot(t, "Corp Root")
	public := newTestRoot(t, "Public Test Root")
	clientOnly := public.issue(t, issuance{cn: "host.example", leaf: true, clientOnly: true})
	unknown := `[talos] controller failed {"controller": "security.TUFTrustedRootController", "error": "Get \"https://` + host + `/root.json\": tls: failed to verify certificate: x509: certificate signed by unknown authority"}`
	check := "Node 10.0.0.11 refused " + host + "'s certificate: certificate signed by unknown authority. Check the Site CA."
	for name, tc := range map[string]struct {
		siteCA string
		chain  []*x509.Certificate
		public *x509.CertPool
		want   string
	}{
		"the whole chain, the Site CA holds its root":       {corp.pem(), certs(leaf, inspect, corp), nil, check},
		"leaf and intermediate, the Site CA holds the root": {corp.pem(), certs(leaf, inspect), nil, check},
		"the Site CA holds a root of the issuer's name":     {remade.pem(), certs(leaf, inspect), nil, check},
		"another root pasted": {
			newTestRoot(t, "Internal-R2").pem(), certs(leaf, inspect, corp), nil,
			"Node 10.0.0.11 refused " + host + "'s certificate. Add its root \"CN=Corp Root\" (SHA-256 " + Fingerprint(corp.cert) + ") to the Site CA.",
		},
		"a presented public root": {
			newTestRoot(t, "Internal-R2").pem(), certs(clientOnly, public), poolOf(certs(public)),
			"Node 10.0.0.11 refused " + host + "'s certificate: certificate signed by unknown authority.",
		},
	} {
		a := &Appliance{sitePublic: tc.public, SiteChains: fakeChains(map[string][]*x509.Certificate{host: tc.chain})}
		r, found := rejectedCertificate([]string{unknown})
		if !found {
			t.Fatalf("%s: no rejection found", name)
		}
		if got := a.refusalLine(t.Context(), &Settings{SiteCA: tc.siteCA}, "10.0.0.11", r); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

// TestAFailedRunLineNamesARootOnlyWhereTheHostPresentsOneTheNodesLack says to check the Site CA
// where the proxy's CA holds the root, where the host leaves out the issuer, which may be a public
// root, and where the Site CA holds the presented root of a chain that fails for another reason.
func TestAFailedRunLineNamesARootOnlyWhereTheHostPresentsOneTheNodesLack(t *testing.T) {
	t.Parallel()
	const host = "tuf-repo-cdn.sigstore.dev"
	proxyRoot := newTestRoot(t, "Proxy Root")
	proxyIssuing := proxyRoot.issue(t, issuance{cn: "Proxy Issuing"})
	proxyLeaf := proxyIssuing.issue(t, issuance{cn: "host.example", leaf: true})
	public := newTestRoot(t, "Public Test Root")
	clientOnly := public.issue(t, issuance{cn: "host.example", leaf: true, clientOnly: true})
	corp := newTestRoot(t, "Corp Root")
	expired := corp.issue(t, issuance{cn: "Corp Inspect", notAfter: time.Now().Add(-time.Minute)})
	underExpired := expired.issue(t, issuance{cn: "host.example", leaf: true})
	other := newTestRoot(t, "Internal-R2").pem()
	withProxy := &Settings{SiteCA: other, Proxy: &Proxy{URL: "http://proxy.example:3128", CABundle: proxyRoot.pem()}}
	unknown := `[talos] controller failed {"controller": "security.TUFTrustedRootController", "error": "Get \"https://` + host + `/root.json\": tls: failed to verify certificate: x509: certificate signed by unknown authority"}`
	check := "Node 10.0.0.11 refused " + host + "'s certificate: certificate signed by unknown authority. Check the Site CA."
	for name, tc := range map[string]struct {
		settings *Settings
		chain    []*x509.Certificate
		public   *x509.CertPool
	}{
		"the proxy's CA holds the presented root":        {withProxy, certs(proxyLeaf, proxyIssuing, proxyRoot), nil},
		"the proxy's CA holds the root left out":         {withProxy, certs(proxyLeaf, proxyIssuing), nil},
		"a public root left out":                         {&Settings{SiteCA: other}, certs(clientOnly), poolOf(certs(public))},
		"the Site CA holds the root above an expired CA": {&Settings{SiteCA: corp.pem()}, certs(underExpired, expired, corp), nil},
	} {
		a := &Appliance{sitePublic: cmp.Or(tc.public, x509.NewCertPool()), SiteChains: fakeChains(map[string][]*x509.Certificate{host: tc.chain})}
		r, found := rejectedCertificate([]string{unknown})
		if !found {
			t.Fatalf("%s: no rejection found", name)
		}
		if got := a.refusalLine(t.Context(), tc.settings, "10.0.0.11", r); got != check {
			t.Errorf("%s:\n got %q\nwant %q", name, got, check)
		}
	}
}

// TestAFailedRunLineReadsThePlainAndTheJSONForm finds the candidate and the reason in a line Talos
// writes as plain text, and in one that quotes a reason other than an unknown authority.
func TestAFailedRunLineReadsThePlainAndTheJSONForm(t *testing.T) {
	t.Parallel()
	const host = "tuf-repo-cdn.sigstore.dev"
	for name, tc := range map[string]struct{ line, want string }{
		"a plain candidate": {
			`[talos] error: Get "https://` + host + `/root.json": tls: failed to verify certificate: x509: certificate signed by unknown authority (possibly because of "crypto/rsa: verification error" while trying to verify candidate authority certificate "Corp Inspection CA")`,
			"Node 10.0.0.11 refused " + host + "'s certificate, signed by a new \"Corp Inspection CA\". Paste the new root.",
		},
		"another name": {
			`[talos] controller failed {"controller": "security.TUFTrustedRootController", "error": "Get \"https://` + host + `/root.json\": tls: failed to verify certificate: x509: certificate is valid for mirror.example, not ` + host + `"}`,
			"Node 10.0.0.11 refused " + host + "'s certificate: certificate is valid for mirror.example, not " + host + ".",
		},
	} {
		r, found := rejectedCertificate([]string{tc.line})
		if !found {
			t.Fatalf("%s: no rejection found", name)
		}
		if got := (&Appliance{}).refusalLine(t.Context(), &Settings{}, "10.0.0.11", r); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

// TestAFailedRunLineCountsOnlyWhatTheNodesTrust says to check the Site CA where the Site CA or the
// proxy's CA holds the presented root under another name, or an intermediate of the chain, and to
// name the root where only a proxy that is switched off holds it.
func TestAFailedRunLineCountsOnlyWhatTheNodesTrust(t *testing.T) {
	t.Parallel()
	const host = "tuf-repo-cdn.sigstore.dev"
	device := newTestRoot(t, "Device Root")
	deviceLeaf := device.issue(t, issuance{cn: "host.example", leaf: true})
	renamed := &x509.Certificate{
		SerialNumber: big.NewInt(3), RawSubject: nameDER(t, "Renamed Root", device.tag), SubjectKeyId: []byte("Renamed Root"),
		NotBefore: device.cert.NotBefore, NotAfter: device.cert.NotAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, renamed, renamed, &device.key.PublicKey, device.key)
	if err != nil {
		t.Fatal(err)
	}
	sameKeyPEM := (&testAuthority{cert: &x509.Certificate{Raw: der}}).pem()
	corp := newTestRoot(t, "Corp Root")
	inspect := corp.issue(t, issuance{cn: "Corp Inspect"})
	corpLeaf := inspect.issue(t, issuance{cn: "host.example", leaf: true})
	other := newTestRoot(t, "Internal-R2").pem()
	enabled := func(bundle string) *Proxy { return &Proxy{URL: "http://proxy.example:3128", CABundle: bundle} }
	unknown := `[talos] controller failed {"controller": "security.TUFTrustedRootController", "error": "Get \"https://` + host + `/root.json\": tls: failed to verify certificate: x509: certificate signed by unknown authority"}`
	check := "Node 10.0.0.11 refused " + host + "'s certificate: certificate signed by unknown authority. Check the Site CA."
	add := func(root *testAuthority) string {
		return "Node 10.0.0.11 refused " + host + "'s certificate. Add its root \"" + subjectOf(root.cert) + "\" (SHA-256 " + Fingerprint(root.cert) + ") to the Site CA."
	}
	for name, tc := range map[string]struct {
		settings *Settings
		chain    []*x509.Certificate
		want     string
	}{
		"a switched-off proxy's CA holds the root":         {&Settings{SiteCA: other, Proxy: &Proxy{CABundle: device.pem()}}, certs(deviceLeaf, device), add(device)},
		"the Site CA holds the root under another name":    {&Settings{SiteCA: sameKeyPEM}, certs(deviceLeaf, device), check},
		"the proxy's CA holds the root under another name": {&Settings{SiteCA: other, Proxy: enabled(sameKeyPEM)}, certs(deviceLeaf, device), check},
		"the Site CA holds an intermediate":                {&Settings{SiteCA: inspect.pem()}, certs(corpLeaf, inspect, corp), check},
		"the proxy's CA holds an intermediate":             {&Settings{SiteCA: other, Proxy: enabled(inspect.pem())}, certs(corpLeaf, inspect, corp), check},
		"nothing holds the intermediate":                   {&Settings{SiteCA: other}, certs(corpLeaf, inspect, corp), add(corp)},
	} {
		a := &Appliance{sitePublic: x509.NewCertPool(), SiteChains: fakeChains(map[string][]*x509.Certificate{host: tc.chain})}
		r, found := rejectedCertificate([]string{unknown})
		if !found {
			t.Fatalf("%s: no rejection found", name)
		}
		if got := a.refusalLine(t.Context(), tc.settings, "10.0.0.11", r); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}
