package certfiles

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"sync/atomic"
	"testing"
	"time"
)

// testNow sits inside every fixture's validity.
var testNow = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// pki is a root, an intermediate below it and a leaf below that.
type pki struct {
	root, inter, leaf *x509.Certificate
	leafKey           crypto.Signer
}

var serial atomic.Int64

func genKey(t *testing.T, kind string) crypto.Signer {
	t.Helper()
	var (
		key crypto.Signer
		err error
	)
	switch kind {
	case "rsa":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	case "p256":
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "p384":
		key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "ed25519":
		_, key, err = ed25519.GenerateKey(rand.Reader)
	default:
		t.Fatalf("unknown key kind %q", kind)
	}
	if err != nil {
		t.Fatalf("generate %s key: %v", kind, err)
	}
	return key
}

// issue signs a certificate for pub, self-signed when parent is nil. A usage of KeyUsageCertSign
// makes a CA.
func issue(t *testing.T, cn string, usage x509.KeyUsage, pub crypto.PublicKey, parent *x509.Certificate, signer crypto.Signer) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial.Add(1)),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Example Org"}},
		NotBefore:             testNow.AddDate(-1, 0, 0),
		NotAfter:              testNow.AddDate(1, 0, 0),
		BasicConstraintsValid: true,
		IsCA:                  usage&x509.KeyUsageCertSign != 0,
		KeyUsage:              usage,
		DNSNames:              []string{"host.example.org"},
	}
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatalf("create certificate %s: %v", cn, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate %s: %v", cn, err)
	}
	return cert
}

// newPKI makes a chain whose leaf key is of the given kind.
func newPKI(t *testing.T, leafKind string) *pki {
	t.Helper()
	rootKey := genKey(t, "p256")
	interKey := genKey(t, "p256")
	leafKey := genKey(t, leafKind)
	root := issue(t, "Example Root", x509.KeyUsageCertSign, rootKey.Public(), nil, rootKey)
	inter := issue(t, "Example Intermediate", x509.KeyUsageCertSign, interKey.Public(), root, rootKey)
	leaf := issue(t, "host.example.org", 0, leafKey.Public(), inter, interKey)
	return &pki{root: root, inter: inter, leaf: leaf, leafKey: leafKey}
}

func rsaKeyOf(t *testing.T, p *pki) *rsa.PrivateKey {
	t.Helper()
	key, ok := p.leafKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("the fixture's key is not RSA")
	}
	return key
}

// sec1DER is the leaf's ECDSA key in SEC1 form.
func sec1DER(t *testing.T, p *pki) []byte {
	t.Helper()
	key, ok := p.leafKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("the fixture's key is not ECDSA")
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal SEC1 key: %v", err)
	}
	return der
}

func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func keyPKCS8(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return der
}

func keyPKCS8PEM(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyPKCS8(t, key)})
}

func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// mustRead reads files and checks what every successful read promises: the pair loads in crypto/tls.
func mustRead(t *testing.T, files []File, passphrase string, roots *x509.CertPool) *Bundle {
	t.Helper()
	bundle, err := Read(files, passphrase, roots, testNow)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, err := tls.X509KeyPair(bundle.CertPEM, bundle.KeyPEM); err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
	return bundle
}

// wantChain checks the leaf and the intermediate come out, in that order, with no root.
func wantChain(t *testing.T, bundle *Bundle, p *pki) {
	t.Helper()
	if len(bundle.Chain) != 2 || !bundle.Leaf.Equal(p.leaf) || !bundle.Chain[0].Equal(p.leaf) || !bundle.Chain[1].Equal(p.inter) {
		t.Fatalf("chain is %d certificates, want leaf then intermediate", len(bundle.Chain))
	}
}
