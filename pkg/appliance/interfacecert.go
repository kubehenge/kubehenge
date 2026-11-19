package appliance

import (
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

// InterfaceCertificateFile is the file in the key directory that holds the certificate an
// administrator gave the interface after the install, as its private key and its chain. The key
// directory is on a volume of its own, which an upgrade leaves alone.
const InterfaceCertificateFile = "interface-certificate.pem"

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
