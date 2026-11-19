package certfiles

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/smallstep/pkcs7"
	"github.com/youmark/pkcs8"
	"software.sslmate.com/src/go-pkcs12"
)

const secret = "hunter2-passphrase"

// allCerts is the leaf, the intermediate and the root as one PEM text.
func allCerts(p *pki) []byte {
	return join(certPEM(p.leaf), certPEM(p.inter), certPEM(p.root))
}

// encryptedKeyPEM is a PKCS#8 key encrypted with PBES2.
func encryptedKeyPEM(t *testing.T, key crypto.Signer, passphrase string) []byte {
	t.Helper()
	der, err := pkcs8.MarshalPrivateKey(key, []byte(passphrase), pkcs8.DefaultOpts)
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der})
}

// legacyKeyPEM is an RSA key in OpenSSL's Proc-Type header form. Go can only write it with the
// deprecated x509.EncryptPEMBlock.
func legacyKeyPEM(t *testing.T, key *rsa.PrivateKey, passphrase string) []byte {
	t.Helper()
	//nolint:staticcheck // the OpenSSL header form has no replacement in the standard library
	block, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), []byte(passphrase), x509.PEMCipherAES256)
	if err != nil {
		t.Fatalf("encrypt legacy key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

func pfxFor(t *testing.T, enc *pkcs12.Encoder, p *pki, passphrase string) []byte {
	t.Helper()
	data, err := enc.Encode(p.leafKey, p.leaf, []*x509.Certificate{p.inter, p.root}, passphrase)
	if err != nil {
		t.Fatalf("encode PKCS#12: %v", err)
	}
	return data
}

func TestFormatFollowsContent(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	der := File{"cert.pem", p.leaf.Raw}
	key := File{"key.pem", keyPKCS8(t, p.leafKey)}
	inter := File{"inter.der", certPEM(p.inter)}
	bundle := mustRead(t, []File{der, key, inter}, "", nil)
	wantChain(t, bundle, p)
}

func TestPEMKeyKinds(t *testing.T) {
	t.Parallel()
	rsaPKI := newPKI(t, "rsa")
	rsaKey := rsaKeyOf(t, rsaPKI)
	ecPKI := newPKI(t, "p256")
	ecDER := sec1DER(t, ecPKI)
	// openssl writes attribute lines before each block.
	bag := []byte("Bag Attributes\n    friendlyName: host.example.org\nsubject=CN=host.example.org\n")
	cases := map[string]struct {
		p   *pki
		key []byte
		pw  string
	}{
		"pkcs8":      {ecPKI, keyPKCS8PEM(t, ecPKI.leafKey), ""},
		"pkcs1":      {rsaPKI, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}), ""},
		"sec1":       {ecPKI, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}), ""},
		"encrypted":  {ecPKI, encryptedKeyPEM(t, ecPKI.leafKey, secret), secret},
		"legacy-enc": {rsaPKI, legacyKeyPEM(t, rsaKey, secret), secret},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			one := join(bag, c.key, bag, certPEM(c.p.leaf), bag, certPEM(c.p.inter))
			bundle := mustRead(t, []File{{"all.pem", one}}, c.pw, nil)
			wantChain(t, bundle, c.p)
		})
	}
}

func TestDERFormats(t *testing.T) {
	t.Parallel()
	rsaPKI := newPKI(t, "rsa")
	rsaKey := rsaKeyOf(t, rsaPKI)
	ecPKI := newPKI(t, "p256")
	ecDER := sec1DER(t, ecPKI)
	cases := map[string]struct {
		p   *pki
		key []byte
	}{
		"pkcs8": {ecPKI, keyPKCS8(t, ecPKI.leafKey)},
		"pkcs1": {rsaPKI, x509.MarshalPKCS1PrivateKey(rsaKey)},
		"sec1":  {ecPKI, ecDER},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := []File{{"leaf.der", c.p.leaf.Raw}, {"inter.der", c.p.inter.Raw}, {"key.der", c.key}}
			wantChain(t, mustRead(t, files, "", nil), c.p)
		})
	}
}

func TestPKCS12(t *testing.T) {
	t.Parallel()
	encoders := map[string]*pkcs12.Encoder{
		"modern": pkcs12.Modern, "legacy-3des": pkcs12.LegacyDES, "legacy-rc2": pkcs12.LegacyRC2,
	}
	for name, enc := range encoders {
		for _, pw := range []string{secret, ""} {
			t.Run(name+"/"+pw, func(t *testing.T) {
				t.Parallel()
				p := newPKI(t, "p256")
				bundle := mustRead(t, []File{{"host.pfx", pfxFor(t, enc, p, pw)}}, pw, nil)
				wantChain(t, bundle, p)
			})
		}
	}
}

func TestPKCS7(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	p7, err := pkcs7.DegenerateCertificate(join(p.leaf.Raw, p.inter.Raw, p.root.Raw))
	if err != nil {
		t.Fatal(err)
	}
	key := File{"key.pem", keyPKCS8PEM(t, p.leafKey)}
	t.Run("der", func(t *testing.T) {
		t.Parallel()
		wantChain(t, mustRead(t, []File{{"chain.p7b", p7}, key}, "", nil), p)
	})
	t.Run("pem", func(t *testing.T) {
		t.Parallel()
		text := pem.EncodeToMemory(&pem.Block{Type: "PKCS7", Bytes: p7})
		wantChain(t, mustRead(t, []File{{"chain.p7c", text}, key}, "", nil), p)
	})
}

func TestEncryptedPKCS8(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"rsa", "p256", "ed25519"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			p := newPKI(t, kind)
			files := []File{{"key.pem", encryptedKeyPEM(t, p.leafKey, secret)}, {"chain.pem", allCerts(p)}}
			wantChain(t, mustRead(t, files, secret, nil), p)
		})
	}
}

func TestKeyTypes(t *testing.T) {
	t.Parallel()
	rsaPKI := newPKI(t, "rsa")
	rsaKey := rsaKeyOf(t, rsaPKI)
	p256 := newPKI(t, "p256")
	p384 := newPKI(t, "p384")
	ed := newPKI(t, "ed25519")
	cases := map[string]struct {
		p   *pki
		key File
	}{
		"rsa":     {rsaPKI, File{"k.pem", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})}},
		"p256":    {p256, File{"k.der", keyPKCS8(t, p256.leafKey)}},
		"p384":    {p384, File{"k.pem", keyPKCS8PEM(t, p384.leafKey)}},
		"ed25519": {ed, File{"k.pem", keyPKCS8PEM(t, ed.leafKey)}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wantChain(t, mustRead(t, []File{c.key, {"c.pem", allCerts(c.p)}}, "", nil), c.p)
		})
	}
}
