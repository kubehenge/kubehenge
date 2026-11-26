// Command tlsca writes a certificate authority shaped like many companies' TLS inspection ones. A
// root whose subject is in PrintableString signs an intermediate whose issuer names the root in
// UTF8String, with the same characters. OpenSSL compares names after normalising them and links
// the intermediate to the root, while Go compares their bytes and finds no parent for it.
//
//	tlsca <directory>
//
// It writes root.pem, the root alone, which a lane gives as its Site CA, and mitmproxy-ca.pem: the
// intermediate's key, the intermediate and the root. mitmproxy signs every certificate with that
// key and sends the intermediate with it.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	keyBits = 2048
	// A lane lasts hours. The margin before now covers clocks a little behind the runner's.
	lifetime = 30 * 24 * time.Hour
	skew     = time.Hour
	// The serial numbers' bits, as RFC 5280 allows at most 20 bytes.
	serialBits = 120
	// The program's name and the directory.
	argCount  = 2
	exitUsage = 2
	pemCert   = "CERTIFICATE"
	// The key is the inspection's, so only its owner reads it.
	dirMode  = 0o700
	fileMode = 0o600
)

// authority is the root and the intermediate it signed, with the intermediate's key.
type authority struct {
	root, intermediate *x509.Certificate
	key                *rsa.PrivateKey
}

func main() {
	if len(os.Args) != argCount {
		fmt.Fprintln(os.Stderr, "usage: tlsca <directory>")
		os.Exit(exitUsage)
	}
	a, err := newAuthority(time.Now())
	if err == nil {
		err = a.write(os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tlsca:", err)
		os.Exit(1)
	}
}

func newAuthority(now time.Time) (*authority, error) {
	rootKey, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, fmt.Errorf("making the root's key: %w", err)
	}
	root, err := sign(caTemplate(now, "Kubehenge e2e inspection root"), nil, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("making the root: %w", err)
	}
	// Go takes an issuer from its parent's raw subject, so a parent carrying the root's name in
	// UTF8String writes it so into the intermediate.
	issuer := *root
	issuer.RawSubject, err = asUTF8(root.RawSubject)
	if err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, fmt.Errorf("making the intermediate's key: %w", err)
	}
	template := caTemplate(now, "Kubehenge e2e inspection intermediate")
	template.MaxPathLenZero = true
	intermediate, err := sign(template, &issuer, &key.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("making the intermediate: %w", err)
	}
	return &authority{root: root, intermediate: intermediate, key: key}, nil
}

func caTemplate(now time.Time, name string) *x509.Certificate {
	return &x509.Certificate{
		Subject:               pkix.Name{Organization: []string{"Kubehenge e2e"}, CommonName: name},
		NotBefore:             now.Add(-skew),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
}

// sign makes the certificate, self-signed when parent is nil, with a random serial number.
func sign(template, parent *x509.Certificate, public *rsa.PublicKey, signer *rsa.PrivateKey) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("drawing a serial number: %w", err)
	}
	template.SerialNumber = serial
	if parent == nil {
		parent = template
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		return nil, fmt.Errorf("signing: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading what was signed: %w", err)
	}
	return cert, nil
}

// asUTF8 is the DER name with every attribute's value as a UTF8String.
func asUTF8(name []byte) ([]byte, error) {
	var rdns pkix.RDNSequence
	rest, err := asn1.Unmarshal(name, &rdns)
	if err != nil || len(rest) > 0 {
		return nil, fmt.Errorf("reading the root's subject: %w", errors.Join(err, errors.New("trailing bytes")))
	}
	for _, rdn := range rdns {
		for i := range rdn {
			value, ok := rdn[i].Value.(string)
			if !ok {
				return nil, fmt.Errorf("the root's subject holds a %T", rdn[i].Value)
			}
			rdn[i].Value = asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(value)}
		}
	}
	encoded, err := asn1.Marshal(rdns)
	if err != nil {
		return nil, fmt.Errorf("writing the root's name in UTF8String: %w", err)
	}
	return encoded, nil
}

func (a *authority) write(dir string) error {
	key, err := x509.MarshalPKCS8PrivateKey(a.key)
	if err != nil {
		return fmt.Errorf("writing the intermediate's key: %w", err)
	}
	var store bytes.Buffer
	for _, block := range []*pem.Block{
		{Type: "PRIVATE KEY", Bytes: key},
		{Type: pemCert, Bytes: a.intermediate.Raw},
		{Type: pemCert, Bytes: a.root.Raw},
	} {
		if err := pem.Encode(&store, block); err != nil {
			return fmt.Errorf("encoding: %w", err)
		}
	}
	root := pem.EncodeToMemory(&pem.Block{Type: pemCert, Bytes: a.root.Raw})
	//nolint:gosec // the directory is the one the caller names.
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	for name, content := range map[string][]byte{"mitmproxy-ca.pem": store.Bytes(), "root.pem": root} {
		//nolint:gosec // a fixed name in the directory the caller names.
		if err := os.WriteFile(filepath.Join(dir, name), content, fileMode); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}
