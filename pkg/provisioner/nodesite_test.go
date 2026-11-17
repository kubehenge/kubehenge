package provisioner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// testCertificate is a self-signed certificate as PEM, valid between the two times.
func testCertificate(t *testing.T, name string, from, to time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: from, NotAfter: to, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestAnAuthorityThatExpiredHoldsNoUnrelatedUpdateBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	expired := testCertificate(t, "old", now.Add(-48*time.Hour), now.Add(-time.Hour))
	valid := testCertificate(t, "valid", now.Add(-time.Hour), now.Add(time.Hour))
	old := withAddons(networkCluster("192.168.1.1", 16), map[string]string{trustedCAsVariable: `[` + quoted(t, expired) + `]`})
	old.Annotations = map[string]string{CiliumClusterIDAnnotation: "7"}
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}

	marked := old.DeepCopy()
	marked.Annotations[AllowDeletionAnnotation] = "dev@pve"
	marked.Labels = map[string]string{"team": "a"}
	marked.Finalizers = append(marked.Finalizers, "example.org/hold")
	if _, err := v.ValidateUpdate(ctx, old, marked); err != nil {
		t.Errorf("annotations, labels and a finalizer on a cluster whose authority expired: %v", err)
	}
	renamed := old.DeepCopy()
	renamed.Spec.Topology.Workers.MachineDeployments[0].Name = "md-1"
	if _, err := v.ValidateUpdate(ctx, old, renamed); err != nil {
		t.Errorf("a pool renamed on a cluster whose authority expired: %v", err)
	}
	added := old.DeepCopy()
	for i := range added.Spec.Topology.Variables {
		if added.Spec.Topology.Variables[i].Name == trustedCAsVariable {
			added.Spec.Topology.Variables[i].Value.Raw = []byte(`[` + quoted(t, expired) + `,` + quoted(t, valid) + `]`)
		}
	}
	if _, err := v.ValidateUpdate(ctx, old, added); err == nil || !strings.Contains(err.Error(), "expired on") {
		t.Errorf("an authority added beside the expired one: %v, want the expired one refused", err)
	}
	// A rule a later Kubehenge adds, which a stored cluster falls short of.
	behind := withAddons(old.DeepCopy(), map[string]string{gitopsVariable: `{"enabled":true,"repository":{"url":"ssh://git@git.example.org/a/b.git"}}`})
	if len(validateGitOps(behind)) == 0 {
		t.Fatal("the GitOps settings meet every rule")
	}
	behindMarked := behind.DeepCopy()
	behindMarked.Annotations[AllowDeletionAnnotation] = "dev@pve"
	if _, err := v.ValidateUpdate(ctx, behind, behindMarked); err != nil {
		t.Errorf("an annotation on a cluster that falls short of a later rule: %v", err)
	}
	moved := old.DeepCopy()
	moved.Annotations[CiliumClusterIDAnnotation] = "8"
	if _, err := v.ValidateUpdate(ctx, old, moved); err == nil || !strings.Contains(err.Error(), "Cilium cluster id") {
		t.Errorf("the Cilium cluster id changed alone: %v, want it refused", err)
	}
}

func quoted(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNodeSiteSettingsAreHeldToCertificatesAndAddresses(t *testing.T) {
	t.Parallel()
	now := time.Now()
	valid := testCertificate(t, "valid", now.Add(-time.Hour), now.Add(time.Hour))
	other := testCertificate(t, "other", now.Add(-time.Hour), now.Add(time.Hour))
	expired := testCertificate(t, "old", now.Add(-48*time.Hour), now.Add(-time.Hour))
	future := testCertificate(t, "new", now.Add(time.Hour), now.Add(48*time.Hour))
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("key")}))
	mirror := func(registry, endpoints, ca string) string {
		extra := ""
		if ca != "" {
			extra = `,"caBundle":` + quoted(t, ca)
		}
		return `{"registry":"` + registry + `","endpoints":` + endpoints + extra + `}`
	}
	for name, tc := range map[string]struct {
		mirrors, cas string
		want         string
	}{
		"nothing": {},
		"a mirror with an authority and a bundle": {
			mirrors: `[` + mirror("docker.io", `["https://m.example.org:5000","http://plain.example.org"]`, valid+other) + `]`,
			cas:     `[` + quoted(t, valid) + `,` + quoted(t, other) + `]`,
		},
		"a wildcard and a path":    {mirrors: `[` + mirror("*", `["https://harbor.example.org/proxy/v2"]`, "") + `]`},
		"a registry with a port":   {mirrors: `[` + mirror("registry.example.org:5000", `["https://m.example.org"]`, "") + `]`},
		"a malformed endpoint":     {mirrors: `[` + mirror("docker.io", `["m.example.org"]`, "") + `]`, want: "is no http:// or https:// address"},
		"an ftp endpoint":          {mirrors: `[` + mirror("docker.io", `["ftp://m.example.org"]`, "") + `]`, want: "is no http:// or https:// address"},
		"credentials in an URL":    {mirrors: `[` + mirror("docker.io", `["https://u:p@m.example.org"]`, "") + `]`, want: "credentials"},
		"a path without /v2":       {mirrors: `[` + mirror("docker.io", `["https://m.example.org/mirror"]`, "") + `]`, want: "does not end in /v2"},
		"a query":                  {mirrors: `[` + mirror("docker.io", `["https://m.example.org/v2?x=1"]`, "") + `]`, want: "query"},
		"a quote in the host":      {mirrors: `[` + mirror("docker.io", `["https://m'.example.org"]`, "") + `]`, want: "characters other than"},
		"an IPv6 host":             {mirrors: `[` + mirror("docker.io", `["https://[::1]:5000"]`, "") + `]`, want: "host"},
		"an upper-case registry":   {mirrors: `[` + mirror("Docker.io", `["https://m.example.org"]`, "") + `]`, want: "lowercase"},
		"no endpoint":              {mirrors: `[` + mirror("docker.io", `[]`, "") + `]`, want: "takes 1 to 4"},
		"five endpoints":           {mirrors: `[` + mirror("docker.io", `["https://a.example.org","https://b.example.org","https://c.example.org","https://d.example.org","https://e.example.org"]`, "") + `]`, want: "takes 1 to 4"},
		"the same endpoint twice":  {mirrors: `[` + mirror("docker.io", `["https://a.example.org","https://a.example.org"]`, "") + `]`, want: "Duplicate"},
		"the same registry twice":  {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, "") + `,` + mirror("docker.io", `["https://b.example.org"]`, "") + `]`, want: "Duplicate"},
		"not a certificate":        {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, "not a certificate") + `]`, want: "holds no PEM certificate"},
		"a private key":            {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, key) + `]`, want: "private key"},
		"an expired authority":     {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, expired) + `]`, want: "certificate 1 (old) expired on"},
		"an authority not yet":     {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, future) + `]`, want: "is valid only from"},
		"text after a certificate": {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, valid+"EOF\n") + `]`, want: "text after its last PEM block"},
		"an authority for http":    {mirrors: `[` + mirror("docker.io", `["http://a.example.org"]`, valid) + `]`, want: "https:// endpoints"},
		"one host, two authorities": {
			mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, valid) + `,` + mirror("ghcr.io", `["https://a.example.org"]`, other) + `]`,
			want:    "one authority per mirror host",
		},
		"one host, one authority": {
			mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, valid) + `,` + mirror("ghcr.io", `["https://a.example.org"]`, valid) + `]`,
		},
		"an expired trusted authority": {cas: `[` + quoted(t, expired) + `]`, want: "expired on"},
		"a trusted key":                {cas: `[` + quoted(t, key) + `]`, want: "private key"},
		"a trusted text":               {cas: `["hello"]`, want: "holds no PEM certificate"},
		"too many trusted":             {cas: `[` + strings.Repeat(quoted(t, valid)+",", 8) + quoted(t, valid) + `]`, want: "Too many"},
		"a bundle of one good and one bad": {
			cas:  `[` + quoted(t, valid+expired) + `]`,
			want: "certificate 2 (old) expired on",
		},
	} {
		set := map[string]string{}
		if tc.mirrors != "" {
			set[registryMirrorsVariable] = tc.mirrors
		}
		if tc.cas != "" {
			set[trustedCAsVariable] = tc.cas
		}
		errs := validateNodeSite(nil, withAddons(cluster(), set), now)
		switch {
		case tc.want == "" && len(errs) > 0:
			t.Errorf("%s: %v", name, errs)
		case tc.want != "" && !strings.Contains(errs.ToAggregate().Error(), tc.want):
			t.Errorf("%s: %v lacks %q", name, errs, tc.want)
		}
	}
}

func TestNodeSiteVariableOfTheWrongTypeIsRefused(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]map[string]string{
		"mirrors":  {registryMirrorsVariable: `{"registry":"docker.io"}`},
		"trusted":  {trustedCAsVariable: `"a certificate"`},
		"mirror":   {registryMirrorsVariable: `[3]`},
		"no value": {},
	} {
		errs := validateNodeSite(nil, withAddons(cluster(), set), time.Now())
		if (name == "no value") != (len(errs) == 0) {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

func TestEgressProxyHoldsAnHTTPSAddressAndTheNodesCharacters(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		v    EgressProxy
		want string
	}{
		"both":                       {EgressProxy{URL: "http://p.example.org:3128", HTTPSURL: "https://q.example.org:3129"}, ""},
		"an IPv6 proxy":              {EgressProxy{URL: "http://[2001:db8::1]:3128"}, ""},
		"exceptions with a wildcard": {EgressProxy{URL: "http://p.example.org", NoProxy: []string{"*.example.org", "fd00::/8", "10.0.0.0/8", ".corp"}}, ""},
		"exceptions without a proxy": {EgressProxy{NoProxy: []string{"a.example.org"}}, ""},
		"https alone":                {EgressProxy{HTTPSURL: "https://q.example.org"}, "httpsUrl comes with url"},
		"direct with https":          {EgressProxy{Disabled: true, URL: "http://p.example.org", HTTPSURL: "https://q.example.org"}, "disabled and url exclude each other"},
		"credentials in https":       {EgressProxy{URL: "http://p.example.org", HTTPSURL: "https://u:p@q.example.org"}, "httpsUrl holds credentials"}, //nolint:gosec // a fake address
		"a quote in the host":        {EgressProxy{URL: "http://p'.example.org"}, "url has a host with characters other than"},
		"a substitution":             {EgressProxy{URL: "http://$(id).example.org"}, "url has a host with characters other than"},
		"a quote in an exception":    {EgressProxy{URL: "http://p.example.org", NoProxy: []string{"a'b"}}, "characters other than"},
		"a substitution in one":      {EgressProxy{URL: "http://p.example.org", NoProxy: []string{"$(id)"}}, "characters other than"},
	} {
		err := checkEgress(&tc.v)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v lacks %q", name, err, tc.want)
		}
	}
}

func TestAddOnsGoThroughTheHTTPSProxyForHTTPSAddresses(t *testing.T) {
	t.Parallel()
	got, _ := Egress{}.resolve(clusterWithRanges(), &EgressProxy{URL: "http://p.example.org:3128", HTTPSURL: "http://q.example.org:3129"})
	vars := map[string]string{}
	for _, v := range got.vars() {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%v", v)
		}
		name, nameOK := m["name"].(string)
		value, valueOK := m[valueKey].(string)
		if !nameOK || !valueOK {
			t.Fatalf("variable %v has no text name and value", m)
		}
		vars[name] = value
	}
	if vars["HTTP_PROXY"] != "http://p.example.org:3128" || vars["https_proxy"] != "http://q.example.org:3129" {
		t.Errorf("variables %v", vars)
	}
	only, _ := Egress{}.resolve(clusterWithRanges(), &EgressProxy{URL: "http://p.example.org:3128"})
	if only.https() != "http://p.example.org:3128" {
		t.Errorf("without httpsUrl, https:// addresses go through %q", only.https())
	}
}

// TestPEMBundleVariablesAreStoredAsCertificatesAlone holds the cluster webhook's rewrite of the
// variables' PEM bundles: a hand-kept bundle on trustedCAs, a registry mirror and the release mirror
// is stored as its certificates alone, the rest of each variable kept. A damaged one is refused by its
// line without the bundle in the message, and an update leaves a variable it does not change as stored.
func TestPEMBundleVariablesAreStoredAsCertificatesAlone(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/curl-cacert.txt")
	if err != nil {
		t.Fatal(err)
	}
	curl := string(raw)
	plain, err := NormaliseCertificateBundle(curl)
	if err != nil || strings.Contains(plain, "#") || strings.Count(plain, "BEGIN CERTIFICATE") != strings.Count(curl, "BEGIN CERTIFICATE") {
		t.Fatalf("the curl excerpt normalised to %q (%v)", plain, err)
	}
	quoted := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	handKept := cluster(
		jsonVar(trustedCAsVariable, `[`+quoted(curl)+`]`),
		jsonVar(registryMirrorsVariable, `[{"registry":"docker.io","endpoints":["https://m.example.org"],"caBundle":`+quoted(curl)+`},{"registry":"ghcr.io","endpoints":["https://g.example.org"]}]`),
		jsonVar(releaseMirrorVariable, `{"url":"https://r.example.org","caBundle":`+quoted(curl)+`}`),
		jsonVar("cni", `"cilium"`),
	)
	if errs := plainBundles(nil, handKept); len(errs) > 0 {
		t.Fatalf("a hand-kept bundle on create: %v", errs)
	}
	cas, _, _ := optionalVariable[[]string](handKept, trustedCAsVariable)
	mirrors, _, _ := optionalVariable[[]RegistryMirror](handKept, registryMirrorsVariable)
	release, _, _ := ClusterReleaseMirror(handKept)
	if len(cas) != 1 || cas[0] != plain {
		t.Errorf("trustedCAs stored as %q, want the certificates alone", cas)
	}
	if len(mirrors) != 2 || mirrors[0].CABundle != plain || mirrors[0].Endpoints[0] != "https://m.example.org" || mirrors[1].Registry != "ghcr.io" || mirrors[1].CABundle != "" {
		t.Errorf("registryMirrors stored as %+v, want the first's caBundle as certificates alone and the rest kept", mirrors)
	}
	if release.CABundle != plain || release.URL != "https://r.example.org" {
		t.Errorf("releaseMirror stored as %+v, want its caBundle as certificates alone", release)
	}

	// A bundle that is already plain, or a variable an update leaves as it was, is not rewritten.
	asStored := cluster(jsonVar(trustedCAsVariable, `[`+quoted(strings.TrimSpace(plain))+`]`), jsonVar(releaseMirrorVariable, `{"url":"https://r.example.org","caBundle":`+quoted(curl)+`}`))
	changed := cluster(jsonVar(trustedCAsVariable, `[`+quoted(strings.TrimSpace(plain))+`]`), jsonVar(releaseMirrorVariable, `{"url":"https://r.example.org","caBundle":`+quoted(curl)+`}`))
	if errs := plainBundles(asStored, changed); len(errs) > 0 || !bytes.Equal(changed.Spec.Topology.Variables[0].Value.Raw, asStored.Spec.Topology.Variables[0].Value.Raw) ||
		!bytes.Equal(changed.Spec.Topology.Variables[1].Value.Raw, asStored.Spec.Topology.Variables[1].Value.Raw) {
		t.Errorf("an update that changes no bundle rewrote %s (%v)", changed.Spec.Topology.Variables, errs)
	}

	damaged := strings.Replace(curl, "-----BEGIN CERTIFICATE-----", "----BEGIN CERTIFICATE-----", 1)
	refused := cluster(jsonVar(registryMirrorsVariable, `[{"registry":"docker.io","endpoints":["https://m.example.org"],"caBundle":`+quoted(damaged)+`}]`))
	errs := plainBundles(nil, refused)
	if len(errs) != 1 || errs[0].Field != "spec.topology.variables[registryMirrors][0].caBundle" ||
		!strings.Contains(errs[0].Error(), "has a damaged BEGIN or END line on line 15") || strings.Contains(errs[0].Error(), "Bundle of CA Root Certificates") {
		t.Errorf("a damaged bundle gave %v, want it refused by its line without its text", errs)
	}
}
