package installer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

// The certificate an administrator gave the interface is kept where the container's backups reach:
// in the directory kubehenge.service gives the interface, on the root disk. The volumes backups
// leave out hold none of it.
func TestTheStoredCertificateIsInTheContainersBackups(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	if out, err := plainOn(t, h, Config{PVEEntry: "no"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	stored := appliance.InterfaceCertificatePath
	create := strings.Fields(h.exec.last("pct create"))
	for i, arg := range create[:len(create)-1] {
		volume := create[i+1]
		if (arg != "--rootfs" && !strings.HasPrefix(arg, "--mp")) || !strings.Contains(volume, ",backup=0") {
			continue
		}
		_, mount, _ := strings.Cut(volume, ",mp=")
		mount, _, _ = strings.Cut(mount, ",")
		if mount == "" || strings.HasPrefix(stored, mount+"/") {
			t.Errorf("%s %s leaves %s out of backups", arg, volume, stored)
		}
	}
	unit, err := os.ReadFile(filepath.Join("..", "..", "appliance", "kubehenge.service"))
	if err != nil {
		t.Fatal(err)
	}
	if dir := path.Dir(stored); !strings.Contains(string(unit), "--appliance-dir "+dir+" ") {
		t.Errorf("kubehenge.service does not give the interface %s as its appliance directory", dir)
	}
}

// certificateUntil is a certificate valid until notAfter, signed by a CA of its own that is not
// this cluster's, as PEM.
func certificateUntil(t *testing.T, notAfter time.Time) string {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example CA"}, NotBefore: notAfter.AddDate(-1, 0, 0), NotAfter: notAfter.AddDate(1, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kubehenge.example.org"}, DNSNames: []string{"kubehenge.example.org"},
		IPAddresses: []net.IP{net.ParseIP("192.0.2.40")}, NotBefore: notAfter.AddDate(-1, 0, 0), NotAfter: notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// While the interface serves a certificate an administrator gave it, an upgrade warns of that one's
// expiry and not of the installer's, which is not served. One that ran out is not served, so the
// warning is the installer's again.
func TestAnUpgradeWarnsOfTheServedCertificate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	const installerWarning, storedWarning = "the appliance's own certificate is valid until", "the certificate an administrator gave the interface is valid until"
	for name, c := range map[string]struct {
		stored string
		want   string
		not    string
	}{
		"a stored certificate":         {stored: certificateUntil(t, now.AddDate(0, 0, 20)), want: storedWarning + " " + now.AddDate(0, 0, 20).Format(time.RFC1123), not: installerWarning},
		"no stored certificate":        {want: installerWarning, not: storedWarning},
		"a stored certificate ran out": {stored: certificateUntil(t, now.AddDate(0, 0, -1)), want: installerWarning, not: storedWarning},
	} {
		h, r, out := upgradeHarness(t)
		h.exec.on("pct exec 100 -- cat /etc/kubehenge/tls.crt", certificateUntil(t, now.AddDate(0, 0, 30)))
		if c.stored != "" {
			h.exec.on("pct exec 100 -- sed -n /-BEGIN CERTIFICATE-/,/-END CERTIFICATE-/p "+appliance.InterfaceCertificatePath, c.stored)
		}
		if _, err := r.Upgrade(ctx, out); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if !strings.Contains(out.String(), c.want) || strings.Contains(out.String(), c.not) {
			t.Errorf("%s: the upgrade says\n%s", name, out)
		}
	}
}
