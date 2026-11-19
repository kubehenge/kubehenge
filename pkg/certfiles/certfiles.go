// Package certfiles reads an administrator's TLS certificate and private key from files in any
// common format and returns one normalised result. Company certificates arrive as PEM, DER, PKCS#7
// or PKCS#12, with encrypted keys, in one file or several, and in any order.
package certfiles

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/smallstep/pkcs7"
	"github.com/youmark/pkcs8"
	"software.sslmate.com/src/go-pkcs12"
)

// maxFileSize stops a wrong file, such as a disk image, from being read into memory.
const maxFileSize = 1 << 20

// ErrPassphrase means encrypted material needs a passphrase, or the one given is wrong.
var ErrPassphrase = errors.New("the passphrase is missing or wrong")

// File is one input file. Name appears only in messages.
type File struct {
	Name string
	Data []byte
}

// Bundle is the certificate and key a set of files holds, ready for tls.X509KeyPair.
type Bundle struct {
	// CertPEM holds the leaf, then its intermediates, as CERTIFICATE blocks.
	CertPEM []byte
	// KeyPEM is the private key as one unencrypted PKCS#8 block.
	KeyPEM []byte
	Leaf   *x509.Certificate
	// Chain is the leaf followed by its intermediates, the certificates in CertPEM.
	Chain []*x509.Certificate
	// Dropped lists the subjects of certificates in the files that are not on the leaf's chain.
	Dropped []string
	// Incomplete is set when the chain stops at a certificate whose issuer is in neither the files
	// nor the roots. MissingIssuer is that issuer's subject.
	Incomplete    bool
	MissingIssuer string
}

// material is everything found in the files so far.
type material struct {
	certs     []*x509.Certificate
	certFiles []string
	key       crypto.Signer
	keyFile   string
	names     []string
}

// Read finds the one private key in files, the certificate that belongs to it and that
// certificate's chain. It tries an empty passphrase before passphrase, so material without a
// passphrase needs none. It returns an error for a leaf that is expired or not yet valid at now,
// and judges nothing else about the leaf. roots, which may be nil, only completes a chain whose
// issuer the files lack.
func Read(files []File, passphrase string, roots *x509.CertPool, now time.Time) (*Bundle, error) {
	var found material
	for _, f := range files {
		found.names = append(found.names, f.Name)
		if err := found.read(f, passphrase); err != nil {
			return nil, err
		}
	}
	if found.key == nil {
		return nil, fmt.Errorf("no private key was found in %s", strings.Join(found.names, ", "))
	}
	leaf := found.leafIndex()
	if leaf < 0 {
		return nil, fmt.Errorf("the private key in %s belongs to none of the certificates in %s",
			found.keyFile, strings.Join(found.names, ", "))
	}
	if err := checkValidity(found.certs[leaf], found.certFiles[leaf], now); err != nil {
		return nil, err
	}
	chain, used, rooted := found.buildChain(leaf)
	bundle := &Bundle{Leaf: found.certs[leaf]}
	for _, i := range chain {
		bundle.Chain = append(bundle.Chain, found.certs[i])
		bundle.CertPEM = append(bundle.CertPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: found.certs[i].Raw})...)
	}
	for i, c := range found.certs {
		if !used[i] {
			bundle.Dropped = append(bundle.Dropped, c.Subject.String())
		}
	}
	if top := bundle.Chain[len(bundle.Chain)-1]; !rooted && !trusted(top, roots, now) {
		bundle.Incomplete = true
		bundle.MissingIssuer = top.Issuer.String()
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(found.key)
	if err != nil {
		return nil, fmt.Errorf("the private key in %s cannot be written as PKCS#8", found.keyFile)
	}
	bundle.KeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return bundle, nil
}

func checkValidity(cert *x509.Certificate, file string, now time.Time) error {
	switch {
	case now.After(cert.NotAfter):
		return fmt.Errorf("the certificate in %s expired on %s", file, cert.NotAfter.UTC().Format(time.RFC3339))
	case now.Before(cert.NotBefore):
		return fmt.Errorf("the certificate in %s is not valid before %s", file, cert.NotBefore.UTC().Format(time.RFC3339))
	}
	return nil
}

// leafIndex finds the certificate for the key. When a renewed certificate keeps its key, the one
// that expires last is the leaf.
func (m *material) leafIndex() int {
	pub, ok := m.key.Public().(interface {
		Equal(other crypto.PublicKey) bool
	})
	if !ok {
		return -1
	}
	leaf := -1
	for i, c := range m.certs {
		if pub.Equal(c.PublicKey) && (leaf < 0 || c.NotAfter.After(m.certs[leaf].NotAfter)) {
			leaf = i
		}
	}
	return leaf
}

// buildChain follows signatures upwards from the leaf. A self-signed root is marked used but left
// out of the chain, and rooted says the walk ended on one (or on a self-signed leaf).
func (m *material) buildChain(leaf int) ([]int, map[int]bool, bool) {
	used := map[int]bool{leaf: true}
	chain := []int{leaf}
	current := leaf
	for !selfSigned(m.certs[current]) {
		next := -1
		for i, c := range m.certs {
			if !used[i] && m.certs[current].CheckSignatureFrom(c) == nil {
				next = i
				break
			}
		}
		if next < 0 {
			return chain, used, false
		}
		used[next] = true
		current = next
		if selfSigned(m.certs[current]) {
			return chain, used, true
		}
		chain = append(chain, current)
	}
	return chain, used, true
}

// selfSigned checks the signature rather than the names, as chain building does.
func selfSigned(cert *x509.Certificate) bool {
	return cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil
}

// trusted reports whether a certificate in roots signed cert.
func trusted(cert *x509.Certificate, roots *x509.CertPool, now time.Time) bool {
	if roots == nil {
		return false
	}
	_, err := cert.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	return err == nil
}

func (m *material) addCert(cert *x509.Certificate, file string) {
	for _, have := range m.certs {
		if bytes.Equal(have.Raw, cert.Raw) {
			return
		}
	}
	m.certs = append(m.certs, cert)
	m.certFiles = append(m.certFiles, file)
}

func (m *material) addKey(key crypto.Signer, file string) error {
	if m.key == nil {
		m.key, m.keyFile = key, file
		return nil
	}
	same, ok := m.key.(interface {
		Equal(other crypto.PrivateKey) bool
	})
	if !ok || !same.Equal(key) {
		return fmt.Errorf("%s and %s hold different private keys, and a certificate has only one", m.keyFile, file)
	}
	return nil
}

// read finds the format from the content, since a file's name says little about it.
func (m *material) read(f File, passphrase string) error {
	if len(f.Data) == 0 {
		return fmt.Errorf("%s is empty, not a certificate or private key", f.Name)
	}
	if len(f.Data) > maxFileSize {
		return fmt.Errorf("%s is larger than 1 MiB, which no certificate or key is", f.Name)
	}
	if block, _ := pem.Decode(f.Data); block != nil {
		return m.readPEM(f, passphrase)
	}
	return m.readDER(f, passphrase)
}

func (m *material) readPEM(f File, passphrase string) error {
	var skipped []string
	found := false
	rest := f.Data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		ok, err := m.addBlock(f.Name, block, passphrase)
		if err != nil {
			return err
		}
		if ok {
			found = true
		} else {
			skipped = append(skipped, block.Type)
		}
	}
	if !found {
		return notUsable(f.Name, skipped)
	}
	return nil
}

// addBlock takes the blocks that hold a certificate or key and reports false for any other.
func (m *material) addBlock(name string, block *pem.Block, passphrase string) (bool, error) {
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return false, fmt.Errorf("%s has a CERTIFICATE block that is not a valid certificate", name)
		}
		m.addCert(cert, name)
	case "PKCS7":
		certs, err := parsePKCS7(block.Bytes)
		if err != nil {
			return false, fmt.Errorf("%s has a PKCS7 block that holds no valid certificates", name)
		}
		for _, c := range certs {
			m.addCert(c, name)
		}
	case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
		return true, m.addPEMKey(name, block, passphrase)
	case "ENCRYPTED PRIVATE KEY":
		key, err := withPassphrases(passphrase, func(pw string) (crypto.Signer, error) {
			parsed, err := pkcs8.ParsePKCS8PrivateKey(block.Bytes, []byte(pw))
			if err != nil {
				return nil, fmt.Errorf("parse: %w", err)
			}
			signer, ok := parsed.(crypto.Signer)
			if !ok {
				return nil, errors.New("not a signing key")
			}
			return signer, nil
		})
		if err != nil {
			return false, fmt.Errorf("%s: %w", name, ErrPassphrase)
		}
		return true, m.addKey(key, name)
	default:
		return false, nil
	}
	return true, nil
}

func (m *material) addPEMKey(name string, block *pem.Block, passphrase string) error {
	if !strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") {
		key, ok := parseKey(block.Bytes)
		if !ok {
			return fmt.Errorf("%s has a %s block that is not a valid private key", name, block.Type)
		}
		return m.addKey(key, name)
	}
	key, err := withPassphrases(passphrase, func(pw string) (crypto.Signer, error) {
		// The legacy header form has no other reader. A wrong passphrase mostly shows as a key that
		// does not parse, so the parse is part of the attempt.
		der, err := x509.DecryptPEMBlock(block, []byte(pw)) //nolint:staticcheck // the OpenSSL header form has no replacement in the standard library
		if err != nil {
			return nil, fmt.Errorf("decrypt: %w", err)
		}
		signer, ok := parseKey(der)
		if !ok {
			return nil, errors.New("not a private key")
		}
		return signer, nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", name, ErrPassphrase)
	}
	return m.addKey(key, name)
}

func (m *material) readDER(f File, passphrase string) error {
	if certs, err := x509.ParseCertificates(f.Data); err == nil && len(certs) > 0 {
		for _, c := range certs {
			m.addCert(c, f.Name)
		}
		return nil
	}
	if key, ok := parseKey(f.Data); ok {
		return m.addKey(key, f.Name)
	}
	bag, err := withPassphrases(passphrase, func(pw string) (pfx, error) {
		key, leaf, chain, err := pkcs12.DecodeChain(f.Data, pw)
		if err != nil {
			return pfx{}, fmt.Errorf("decode: %w", err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return pfx{}, errors.New("not a signing key")
		}
		return pfx{signer, append([]*x509.Certificate{leaf}, chain...)}, nil
	})
	switch {
	case err == nil:
		for _, c := range bag.certs {
			m.addCert(c, f.Name)
		}
		return m.addKey(bag.key, f.Name)
	case errors.Is(err, pkcs12.ErrIncorrectPassword), errors.Is(err, pkcs12.ErrDecryption):
		return fmt.Errorf("%s: %w", f.Name, ErrPassphrase)
	}
	if certs, err := parsePKCS7(f.Data); err == nil {
		for _, c := range certs {
			m.addCert(c, f.Name)
		}
		return nil
	}
	if _, err := x509.ParsePKIXPublicKey(f.Data); err == nil {
		return fmt.Errorf("%s is a public key, not a certificate or private key", f.Name)
	}
	return notUsable(f.Name, nil)
}

// pfx is what a PKCS#12 file holds: one key, and the leaf followed by its chain.
type pfx struct {
	key   crypto.Signer
	certs []*x509.Certificate
}

// withPassphrases tries an empty passphrase, which unencrypted and empty-password material needs,
// and then passphrase. It returns the last attempt's error.
func withPassphrases[T any](passphrase string, try func(string) (T, error)) (T, error) {
	got, err := try("")
	if err != nil && passphrase != "" {
		got, err = try(passphrase)
	}
	return got, err
}

func parseKey(der []byte) (crypto.Signer, bool) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		signer, ok := k.(crypto.Signer)
		return signer, ok
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, true
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, true
	}
	return nil, false
}

func parsePKCS7(der []byte) ([]*x509.Certificate, error) {
	p7, err := pkcs7.Parse(der)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(p7.Certificates) == 0 {
		return nil, errors.New("no certificates")
	}
	return p7.Certificates, nil
}

func notUsable(name string, skipped []string) error {
	if len(skipped) > 0 {
		return fmt.Errorf("%s holds %s, not a certificate or private key", name, strings.Join(skipped, ", "))
	}
	return fmt.Errorf("%s is not a certificate or private key in a format this reads (PEM, DER, PKCS#7 or PKCS#12)", name)
}
