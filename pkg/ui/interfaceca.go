package ui

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

// certificateApplianceCA is the source of a certificate the appliance's own CA signed.
const certificateApplianceCA = "appliance-ca"

const (
	// applianceCAYears is how long the appliance's CA holds, so that trusting it once lasts.
	applianceCAYears = 10
	// noDNSName is the only DNS name a CA made for addresses alone permits, which no name can be.
	noDNSName = "invalid"
	// bitsPerByte turns an address's length into the length of a mask that covers it alone.
	bitsPerByte = 8

	pemCertificate = "CERTIFICATE"
	pemPrivateKey  = "PRIVATE KEY"
)

// applianceCA is the CA an administrator may have the appliance keep, to trust once instead of each
// certificate. Its name constraints hold it to the names and addresses it was made for, so trusting
// it vouches for nothing else.
type applianceCA struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte
}

// applianceCAView is the appliance's CA as administrators see it, to trust it.
type applianceCAView struct {
	Subject     string    `json:"subject"`
	Fingerprint string    `json:"fingerprint"`
	NotAfter    time.Time `json:"notAfter"`
	// Permits are the names and addresses the CA may sign for.
	Permits []string `json:"permits"`
	PEM     string   `json:"pem"`
}

func (ca *applianceCA) view() *applianceCAView {
	permits := slices.DeleteFunc(slices.Clone(ca.cert.PermittedDNSDomains), func(d string) bool { return d == noDNSName })
	for _, r := range ca.cert.PermittedIPRanges {
		permits = append(permits, r.IP.String())
	}
	return &applianceCAView{
		Subject: ca.cert.Subject.String(), Fingerprint: appliance.Fingerprint(ca.cert), NotAfter: ca.cert.NotAfter.UTC(),
		Permits: permits, PEM: string(ca.pem),
	}
}

// loadCA reads the appliance's CA, nil when it has none.
func (c *InterfaceCertificate) loadCA() (*applianceCA, error) {
	if c.memory {
		return c.memCA, nil
	}
	raw, err := os.ReadFile(c.caPath()) //nolint:gosec // --appliance-dir and a constant name, nothing from the request
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // no CA is no error
	}
	if err != nil {
		return nil, fmt.Errorf("reading the appliance's CA: %w", err)
	}
	ca := &applianceCA{}
	for block, rest := pem.Decode(raw); block != nil; block, rest = pem.Decode(rest) {
		switch block.Type {
		case pemCertificate:
			ca.cert, err = x509.ParseCertificate(block.Bytes)
			ca.pem = pem.EncodeToMemory(block)
		case pemPrivateKey:
			var key any
			if key, err = x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				ca.key, _ = key.(crypto.Signer) //nolint:revive // a key that cannot sign is left nil, which the check below refuses
			}
		}
		if err != nil {
			return nil, fmt.Errorf("reading the appliance's CA: %w", err)
		}
	}
	if ca.cert == nil || ca.key == nil {
		return nil, errors.New("reading the appliance's CA: it lacks its certificate or key")
	}
	return ca, nil
}

// keepCA stores the CA, its key first.
func (c *InterfaceCertificate) keepCA(ca *applianceCA) error {
	if c.memory {
		c.memCA = ca
		return nil
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(ca.key)
	if err != nil {
		return fmt.Errorf("writing the CA's key: %w", err)
	}
	return c.write(appliance.InterfaceCAFile, slices.Concat(pem.EncodeToMemory(&pem.Block{Type: pemPrivateKey, Bytes: keyDER}), ca.pem))
}

func (c *InterfaceCertificate) caPath() string {
	return filepath.Join(c.dir, appliance.InterfaceCAFile)
}

// forgetCA removes the CA, so that its key is gone with the certificates it signed.
func (c *InterfaceCertificate) forgetCA() error {
	if c.memory {
		c.memCA = nil
		return nil
	}
	if err := c.fileOps().Remove(c.caPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err //nolint:wrapcheck // the file operations name what failed
	}
	return nil
}

// newApplianceCA makes a CA held to the names and addresses of old.
func newApplianceCA(old *x509.Certificate, now time.Time) (*applianceCA, error) {
	key, serial, err := newKeyAndSerial()
	if err != nil {
		return nil, err
	}
	start := now.Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "Kubehenge appliance CA", Organization: []string{"Kubehenge"}},
		NotBefore: start, NotAfter: start.AddDate(applianceCAYears, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
	}
	for _, name := range old.DNSNames {
		tmpl.PermittedDNSDomains = append(tmpl.PermittedDNSDomains, strings.TrimPrefix(name, "*."))
	}
	for _, ip := range old.IPAddresses {
		bits := bitsPerByte * net.IPv6len
		if v4 := ip.To4(); v4 != nil {
			ip, bits = v4, bitsPerByte*net.IPv4len
		}
		tmpl.PermittedIPRanges = append(tmpl.PermittedIPRanges, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	// A kind of name the certificate has none of is shut out, since a kind without a constraint is
	// open to every name. Nothing can hold a name under .invalid (RFC 2606).
	if len(tmpl.PermittedDNSDomains) == 0 {
		tmpl.PermittedDNSDomains = []string{noDNSName}
	}
	if len(tmpl.PermittedIPRanges) == 0 {
		tmpl.ExcludedIPRanges = []*net.IPNet{
			{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, bitsPerByte*net.IPv4len)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, bitsPerByte*net.IPv6len)},
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("making the CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading the CA made: %w", err)
	}
	return &applianceCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: der})}, nil
}

// signedByCA is a certificate for the names of old that the appliance's CA signed. The CA is made
// first where there is none, or where the one there may not sign for these names or has expired.
// The caller holds mu.
func (c *InterfaceCertificate) signedByCA(old *x509.Certificate, now time.Time) (*certfiles.Bundle, error) {
	if len(old.DNSNames) == 0 && len(old.IPAddresses) == 0 {
		return nil, errNoNames
	}
	ca, err := c.loadCA()
	if err != nil {
		return nil, err
	}
	if ca != nil {
		var b *certfiles.Bundle
		if b, err = ca.sign(old, now); err != nil {
			return nil, err
		}
		if ca.vouches(b.Leaf, now) {
			return b, nil
		}
	}
	if ca, err = newApplianceCA(old, now); err != nil {
		return nil, err
	}
	if err = c.keepCA(ca); err != nil {
		return nil, err
	}
	return ca.sign(old, now)
}

func (ca *applianceCA) sign(old *x509.Certificate, now time.Time) (*certfiles.Bundle, error) {
	tmpl, key, err := leafFor(old, now)
	if err != nil {
		return nil, err
	}
	if tmpl.NotAfter.After(ca.cert.NotAfter) {
		tmpl.NotAfter = ca.cert.NotAfter
	}
	return bundleOf(tmpl, ca.cert, ca.key, key, ca.cert)
}

// vouches tells whether a browser that trusts the CA accepts leaf, its name constraints included.
func (ca *applianceCA) vouches(leaf *x509.Certificate, now time.Time) bool {
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err == nil
}

// renewDue tells whether a certificate the appliance's CA signed has under a third of its lifetime
// left, when it is signed again.
func renewDue(leaf *x509.Certificate, now time.Time) bool {
	return leaf.NotAfter.Sub(now) < leaf.NotAfter.Sub(leaf.NotBefore)/3
}

// renewed is the stored certificate, signed again by the appliance's CA once that is due. It stays
// the one stored when it is no certificate of the CA's, or the signing fails. The caller holds mu.
func (c *InterfaceCertificate) renewed(stored *servedCertificate, now time.Time, log *slog.Logger) *servedCertificate {
	if stored.source != certificateApplianceCA || !renewDue(stored.pair.Leaf, now) {
		return stored
	}
	b, err := c.signedByCA(stored.pair.Leaf, now)
	var next *servedCertificate
	if err == nil {
		next, err = newServed(b, certificateApplianceCA, "")
	}
	if err == nil {
		err = c.store(b, certificateApplianceCA, "")
	}
	if err != nil {
		log.Error("the interface certificate was not signed again by the appliance's CA", "reason", err)
		return stored
	}
	return next
}

// makeApplianceCACertificate replaces the certificate with one the appliance's CA signs for the
// names and addresses the served certificate covers.
func (s *Server) makeApplianceCACertificate(w http.ResponseWriter, r *http.Request, u user) {
	c := s.certificateToChange(w, r, u)
	if c == nil {
		return
	}
	if !lockChange(w, c) {
		return
	}
	defer c.mu.Unlock()
	b, err := c.signedByCA(c.served().pair.Leaf, s.now())
	if errors.Is(err, errNoNames) {
		writeCertificateError(w, http.StatusUnprocessableEntity, "no-names", "The certificate being served names no DNS name or address, so the appliance's CA has nothing to sign for. Upload one", nil)
		return
	}
	if err != nil {
		s.changeFailed(w, r, err)
		return
	}
	served, err := c.replace(b, certificateApplianceCA)
	if err != nil {
		s.changeFailed(w, r, err)
		return
	}
	s.changed(r, u, "interface certificate signed by the appliance's CA", served)
	writeJSON(w, http.StatusOK, s.certificateViewOf(r.Context(), c))
}
