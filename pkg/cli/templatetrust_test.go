package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// TestTemplateBuildTrustsTheRootAnInspectingIntermediateRenames gives the build only a root whose
// name is in PrintableString, as the appliance is given it. The inspecting device's intermediate
// names that root in UTF8String, which Go refuses. The build's authorities and its own downloads
// must accept the chain.
func TestTemplateBuildTrustsTheRootAnInspectingIntermediateRenames(t *testing.T) {
	t.Parallel()
	rootKey, root := inspectionCert(t, "Inspect Root", asn1.TagPrintableString, nil, nil)
	issuer := *root
	issuer.RawSubject = inspectionName(t, "Inspect Root", asn1.TagUTF8String)
	interKey, inter := inspectionCert(t, "Inspect Intermediate", asn1.TagPrintableString, &issuer, rootKey)
	leafKey, leaf := inspectionCert(t, "127.0.0.1", asn1.TagPrintableString, inter, interKey, net.IPv4(127, 0, 0, 1))
	chain := []*x509.Certificate{leaf, inter}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	if goAccepts(chain, rootPEM) == nil {
		t.Fatal("Go accepts the chain with the root alone, so the test proves nothing")
	}

	b := &templates.Builder{Proxy: &templates.GuestProxy{CABundle: rootPEM}, Sources: templates.DefaultSources(nil)}
	chains := func(context.Context, *appliance.Settings, string) ([]*x509.Certificate, error) { return chain, nil }
	if err := trustSite(t.Context(), b, chains); err != nil {
		t.Fatal(err)
	}
	if err := goAccepts(chain, b.Proxy.CABundle); err != nil {
		t.Errorf("the build's authorities: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw, inter.Raw}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.Sources.HTTP.Do(req)
	if err != nil {
		t.Fatalf("the build's downloads: %v", err)
	}
	_ = resp.Body.Close()
}

// inspectionName is the name with one common name of the ASN.1 string type tag.
func inspectionName(t *testing.T, cn string, tag int) []byte {
	t.Helper()
	name := []pkix.RelativeDistinguishedNameSET{{{Type: asn1.ObjectIdentifier{2, 5, 4, 3}, Value: asn1.RawValue{Tag: tag, Bytes: []byte(cn)}}}}
	raw, err := asn1.Marshal(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// inspectionCert is a certificate named cn in tag, signed by parent's key, or by its own without a
// parent: a server leaf for the addresses ips, or a CA without them.
func inspectionCert(t *testing.T, cn string, tag int, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, ips ...net.IP) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), RawSubject: inspectionName(t, cn, tag),
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: len(ips) == 0, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	if len(ips) > 0 {
		tmpl.KeyUsage, tmpl.ExtKeyUsage, tmpl.IPAddresses = x509.KeyUsageDigitalSignature, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, ips
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

// goAccepts verifies the chain as Go's TLS client does, with the bundle as its roots.
func goAccepts(chain []*x509.Certificate, bundle string) error {
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(bundle))
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
	return err
}
