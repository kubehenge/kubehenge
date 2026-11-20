package appliance

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

const (
	// InterfaceCertificateFile is the file in the appliance's directory that holds the certificate an
	// administrator gave the interface after the install, as its private key and its chain. That
	// directory is on the container's root disk, which its backups hold, so a restore keeps it.
	InterfaceCertificateFile = "interface-certificate.pem"
	// InterfaceCertificatePath is InterfaceCertificateFile in the directory kubehenge.service gives
	// the interface as --appliance-dir.
	InterfaceCertificatePath = "/var/lib/kubehenge/" + InterfaceCertificateFile
	// InstallerCertificatePath is the certificate the installer made, which the interface serves
	// while no other is stored.
	InstallerCertificatePath = "/etc/kubehenge/tls.crt"
)

// StoredInterfaceCertificate reads the certificate the interface serves in place of the installer's
// when the file at path reads cleanly. The interface and the upgrade's health check both decide by
// this, so that they agree on which certificate is served.
func StoredInterfaceCertificate(path string, now time.Time) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the stored certificate's own path
	if err != nil {
		return nil, fmt.Errorf("reading the stored certificate: %w", err)
	}
	b, err := certfiles.Read([]certfiles.File{{Name: InterfaceCertificateFile, Data: raw}}, "", nil, now)
	if err != nil {
		return nil, fmt.Errorf("reading the stored certificate: %w", err)
	}
	return b.Leaf, nil
}

// ServedCertificate is the certificate the interface serves: the one in storedFile while it reads
// cleanly, else the installer's in certFile. It returns the file it came from too.
func ServedCertificate(certFile, storedFile string, now time.Time) (*x509.Certificate, string, error) {
	if storedFile != "" {
		if leaf, err := StoredInterfaceCertificate(storedFile, now); err == nil {
			return leaf, storedFile, nil
		}
	}
	raw, err := os.ReadFile(certFile) //nolint:gosec // the installer's certificate
	if err != nil {
		return nil, "", fmt.Errorf("the interface's certificate: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, "", fmt.Errorf("the interface's certificate %s holds no PEM", certFile)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("the interface's certificate: %w", err)
	}
	return leaf, certFile, nil
}

// Fingerprint is a certificate's SHA-256 as people compare it: upper-case hex pairs joined by
// colons.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
