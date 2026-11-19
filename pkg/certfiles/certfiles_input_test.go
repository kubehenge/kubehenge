package certfiles

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/smallstep/pkcs7"
	"software.sslmate.com/src/go-pkcs12"
)

func TestPassphraseErrors(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "rsa")
	rsaKey := rsaKeyOf(t, p)
	chain := File{"chain.pem", allCerts(p)}
	encrypted := map[string]File{
		"pfx":       {"host.pfx", pfxFor(t, pkcs12.Modern, p, secret)},
		"pkcs8":     {"key.pem", encryptedKeyPEM(t, p.leafKey, secret)},
		"legacy":    {"key.pem", legacyKeyPEM(t, rsaKey, secret)},
		"pfx-3des":  {"host.pfx", pfxFor(t, pkcs12.LegacyDES, p, secret)},
		"pfx-empty": {"empty.pfx", pfxFor(t, pkcs12.Modern, p, "")},
	}
	for name, f := range encrypted {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := []File{f, chain}
			if name == "pfx-empty" {
				// A passphrase that the file does not need must not get in the way.
				wantChain(t, mustRead(t, files, secret, nil), p)
				return
			}
			for _, wrong := range []string{"", "not-the-passphrase"} {
				_, err := Read(files, wrong, nil, testNow)
				if !errors.Is(err, ErrPassphrase) || !strings.Contains(err.Error(), f.Name) {
					t.Fatalf("passphrase %q gave %v, want ErrPassphrase naming %s", wrong, err, f.Name)
				}
			}
			wantChain(t, mustRead(t, files, secret, nil), p)
		})
	}
}

// A legacy key block that decrypts to something that is no key reads as a wrong passphrase, since
// that is how a wrong passphrase mostly shows.
func TestLegacyKeyThatDecryptsToGarbage(t *testing.T) {
	t.Parallel()
	//nolint:staticcheck // the OpenSSL header form has no replacement in the standard library
	block, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", []byte("not a key"), []byte(secret), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Read([]File{{"key.pem", pem.EncodeToMemory(block)}}, secret, nil, testNow)
	if !errors.Is(err, ErrPassphrase) {
		t.Fatalf("error %v is not ErrPassphrase", err)
	}
}

func TestGarbageInput(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "host.example.org"}}, p.leafKey)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(p.leafKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	pfx := pfxFor(t, pkcs12.Modern, p, "")
	noCerts, err := pkcs7.DegenerateCertificate([]byte{})
	if err != nil {
		t.Fatal(err)
	}
	x25519, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x25519DER, err := x509.MarshalPKCS8PrivateKey(x25519)
	if err != nil {
		t.Fatal(err)
	}
	const notKey = "not a certificate or private key"
	cases := map[string]struct {
		data []byte
		want string
	}{
		"empty.pem":     {[]byte{}, "is empty, " + notKey},
		"notes.txt":     {[]byte("the certificate is on the other server\n"), "is " + notKey},
		"cut.pfx":       {pfx[:len(pfx)/2], "is " + notKey},
		"request.csr":   {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), "holds CERTIFICATE REQUEST, " + notKey},
		"public.pem":    {pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), "holds PUBLIC KEY, " + notKey},
		"public.der":    {pubDER, "is a public key, " + notKey},
		"no-certs.p7b":  {noCerts, "is " + notKey},
		"bad-p7.pem":    {pem.EncodeToMemory(&pem.Block{Type: "PKCS7", Bytes: []byte("not asn1")}), "has a PKCS7 block that holds no valid certificates"},
		"x25519.pem":    {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x25519DER}), "holds a private key of a type that cannot sign"},
		"bad-block.pem": {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not asn1")}), "has a CERTIFICATE block that is not a valid certificate"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Read([]File{{name, c.data}}, "", nil, testNow)
			if err == nil || !strings.Contains(err.Error(), name+" "+c.want) {
				t.Fatalf("error %v does not say %s %s", err, name, c.want)
			}
		})
	}
}

// TestTruncatedInputsDoNotPanic cuts every format short. It only runs the reader, since a cut file
// may still be readable.
func TestTruncatedInputsDoNotPanic(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	p7, err := pkcs7.DegenerateCertificate(join(p.leaf.Raw, p.inter.Raw, p.root.Raw))
	if err != nil {
		t.Fatal(err)
	}
	samples := [][]byte{
		join(keyPKCS8PEM(t, p.leafKey), allCerts(p)),
		p.leaf.Raw,
		keyPKCS8(t, p.leafKey),
		encryptedKeyPEM(t, p.leafKey, secret),
		pfxFor(t, pkcs12.Modern, p, secret),
		pfxFor(t, pkcs12.LegacyRC2, p, ""),
		p7,
		pem.EncodeToMemory(&pem.Block{Type: "PKCS7", Bytes: p7}),
	}
	for _, sample := range samples {
		for n := 0; n < len(sample); n += 1 + len(sample)/97 {
			cut := File{"cut", sample[:n]}
			_, _ = Read([]File{cut, {"chain.pem", allCerts(p)}}, secret, nil, testNow)
			flipped := append([]byte(nil), sample...)
			flipped[n] ^= 0xff
			_, _ = Read([]File{{"flipped", flipped}}, secret, nil, testNow)
		}
	}
}

func TestOversizeFileRefused(t *testing.T) {
	t.Parallel()
	_, err := Read([]File{{"huge.bin", make([]byte, 1<<20+1)}}, "", nil, testNow)
	if err == nil || !strings.Contains(err.Error(), "huge.bin") || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("error %v does not name the file and the limit", err)
	}
}

func TestErrorsLeakNoSecrets(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	other := newPKI(t, "p256")
	keyDER := keyPKCS8(t, p.leafKey)
	fragment := base64.StdEncoding.EncodeToString(keyDER)[:32]
	pfx := pfxFor(t, pkcs12.Modern, p, "another-secret")
	scenarios := map[string][]File{
		"wrong passphrase pkcs8": {{"key.pem", encryptedKeyPEM(t, p.leafKey, "another-secret")}},
		"wrong passphrase pfx":   {{"host.pfx", pfx}},
		"cut pfx":                {{"host.pfx", pfx[:len(pfx)-9]}},
		"no key":                 {{"chain.pem", allCerts(p)}},
		"two keys":               {{"a.pem", keyPKCS8PEM(t, p.leafKey)}, {"b.pem", keyPKCS8PEM(t, other.leafKey)}},
		"foreign key":            {{"key.pem", keyPKCS8PEM(t, p.leafKey)}, {"chain.pem", allCerts(other)}},
		"damaged key":            {{"key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER[:40]})}},
	}
	for name, files := range scenarios {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Read(files, secret, nil, testNow)
			if err == nil {
				t.Fatal("Read succeeded")
			}
			for _, leak := range []string{secret, fragment, base64.StdEncoding.EncodeToString(keyDER[:40])[:32]} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error %q holds %q", err, leak)
				}
			}
		})
	}
}
