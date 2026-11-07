package provisioner

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
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
		"an upper-case registry":   {mirrors: `[` + mirror("Docker.io", `["https://m.example.org"]`, "") + `]`, want: "lower-case"},
		"no endpoint":              {mirrors: `[` + mirror("docker.io", `[]`, "") + `]`, want: "takes 1 to 4"},
		"five endpoints":           {mirrors: `[` + mirror("docker.io", `["https://a.example.org","https://b.example.org","https://c.example.org","https://d.example.org","https://e.example.org"]`, "") + `]`, want: "takes 1 to 4"},
		"the same endpoint twice":  {mirrors: `[` + mirror("docker.io", `["https://a.example.org","https://a.example.org"]`, "") + `]`, want: "Duplicate"},
		"the same registry twice":  {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, "") + `,` + mirror("docker.io", `["https://b.example.org"]`, "") + `]`, want: "Duplicate"},
		"not a certificate":        {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, "not a certificate") + `]`, want: "holds no PEM certificate"},
		"a private key":            {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, key) + `]`, want: "private key"},
		"an expired authority":     {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, expired) + `]`, want: "certificate 1 (old) expired on"},
		"an authority not yet":     {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, future) + `]`, want: "is valid only from"},
		"text after a certificate": {mirrors: `[` + mirror("docker.io", `["https://a.example.org"]`, valid+"EOF\n") + `]`, want: "text besides"},
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
		errs := validateNodeSite(withAddons(cluster(), set), now)
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
		errs := validateNodeSite(withAddons(cluster(), set), time.Now())
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
		name, _ := m["name"].(string)
		value, _ := m[valueKey].(string)
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
