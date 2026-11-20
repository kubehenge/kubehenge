package certfiles

import (
	"crypto"
	"crypto/x509"
	"fmt"
	"time"
)

func checkValidity(cert *x509.Certificate, file string, now time.Time) error {
	switch {
	case now.After(cert.NotAfter):
		return fmt.Errorf("%s holds a certificate that expired on %s", file, cert.NotAfter.UTC().Format(time.RFC3339))
	case now.Before(cert.NotBefore):
		return fmt.Errorf("%s holds a certificate that is not valid before %s", file, cert.NotBefore.UTC().Format(time.RFC3339))
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

// trusted reports whether a certificate in roots signed cert. A nil pool means no roots, where
// Verify would fall back to the system's.
func trusted(cert *x509.Certificate, roots *x509.CertPool, now time.Time) bool {
	if roots == nil {
		return false
	}
	_, err := cert.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	return err == nil
}
