package ui

import (
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

const movedHost = "moved.example.org"

func moveTo(t *testing.T, a *browser, name string) uploadView {
	t.Helper()
	return certificateOf(t, a.do("PUT", "/api/appliance/certificate/address", `{"name":"`+name+`"}`))
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	raw, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A self-signed certificate is made again for the new name, without the old one, and the address
// and Headlamp's callback at it outlive a restart.
func TestTheAddressMovesASelfSignedCertificateWithIt(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	c.HeadlampPort = "8443"
	certificateOf(t, a.do("POST", "/api/appliance/certificate/self-signed", ""))
	v := moveTo(t, a, "Moved.Example.org.")
	if v.PublicURL != "https://"+movedHost || v.Source != certificateSelfSigned || v.PublicNameCovered == nil || !*v.PublicNameCovered {
		t.Errorf("view = %+v", v)
	}
	if leaf := c.served().pair.Leaf; !slices.Equal(leaf.DNSNames, []string{movedHost, "kubehenge"}) || leaf.Subject.CommonName != movedHost || len(leaf.IPAddresses) != 1 {
		t.Errorf("made again for %v %v, %s", leaf.DNSNames, leaf.IPAddresses, leaf.Subject.CommonName)
	}
	if got := fileText(t, filepath.Join(c.dir, appliance.PublicURLFile)); got != "https://"+movedHost+"\n" {
		t.Errorf("stored %q", got)
	}
	if got := fileText(t, filepath.Join(c.dir, appliance.HeadlampEnvFile)); got != "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://"+movedHost+":8443/oidc-callback\n" {
		t.Errorf("Headlamp's file holds %q", got)
	}
	next := restartedAt(t, c, h.clock.Add(time.Hour))
	if got := next.address.Load(); got == nil || *got != "https://"+movedHost {
		t.Errorf("after a restart the address is %v", got)
	}
	h.server.Certificate = next
	if got := h.server.notifyLink("/clusters"); got != "https://"+movedHost+"/clusters" {
		t.Errorf("links lead to %s", got)
	}
}

// A certificate of the appliance's CA is signed again for the new name, by a CA that permits it.
func TestTheAddressMovesTheAppliancesCACertificateWithIt(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	certificateOf(t, a.do("POST", "/api/appliance/certificate/appliance-ca", ""))
	v := moveTo(t, a, movedHost)
	ca := caOf(t, v.ApplianceCA)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := c.served().pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: movedHost, CurrentTime: h.clock}); err != nil || v.Source != certificateApplianceCA {
		t.Errorf("%s from %s: %v", movedHost, v.Source, err)
	}
}

// With ACME set, the new name gets a certificate ordered for it.
func TestTheAddressHasACMEOrderForIt(t *testing.T) {
	t.Parallel()
	r := administeredWithACME(t)
	acmeView(t, r.a.do("PUT", "/api/appliance/certificate/acme", stagingRequest), http.StatusAccepted)
	waitFor(t, "the first certificate", func() bool { return servedSource(r.c) == certificateACME })
	if v := moveTo(t, r.a, movedHost); v.ACME == nil || v.ACME.PublicName != movedHost {
		t.Errorf("ACME after the move: %+v", v.ACME)
	}
	waitFor(t, "a certificate for "+movedHost, func() bool {
		return servedSource(r.c) == certificateACME && r.c.served().pair.Leaf.VerifyHostname(movedHost) == nil
	})
}

// A certificate the appliance cannot make again stays, and the page says it does not cover the new
// name. One for the new name is then taken.
func TestTheAddressLeavesAnUploadedCertificate(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	before := c.fingerprint()
	v := moveTo(t, a, movedHost)
	if c.fingerprint() != before || v.PublicNameCovered == nil || *v.PublicNameCovered {
		t.Errorf("the certificate changed, or covers %s: %+v", movedHost, v)
	}
	moved := issueCertificate(t, h.clock.Add(-time.Hour), h.clock.AddDate(1, 0, 0), []string{movedHost}, certificateAddress.String())
	expect(t, a.upload(filePart("moved.pem", moved.CertPEM), filePart("moved.key", moved.KeyPEM)), http.StatusOK)
}

// An address keeps the port of the one before, IPv6 in brackets, and anything that is no DNS name
// or address is refused with nothing changed.
func TestTheAddressTakesNamesAndAddressesOnly(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	h.server.PublicURL = "https://" + certificateHost + ":8006"
	for _, tc := range [][2]string{
		{"192.0.2.40", "https://192.0.2.40:8006"},
		{"2001:db8::40", "https://[2001:db8::40]:8006"},
		{"[2001:DB8::41]", "https://[2001:db8::41]:8006"},
		{" kubehenge-2 ", "https://kubehenge-2:8006"},
		{movedHost, "https://moved.example.org:8006"},
	} {
		if v := moveTo(t, a, tc[0]); v.PublicURL != tc[1] {
			t.Errorf("%q: %s, want %s", tc[0], v.PublicURL, tc[1])
		}
	}
	for _, name := range []string{"", "bank example", "*.example.org", "fe80::1%eth0", "https://moved.example.org", "a_b.example.org"} {
		refusal(t, a.do("PUT", "/api/appliance/certificate/address", `{"name":"`+name+`"}`), http.StatusUnprocessableEntity, "bad-address")
	}
	if got := h.server.PublicAddress(); got != "https://moved.example.org:8006" || c.address.Load() == nil {
		t.Errorf("a refusal changed the address to %s", got)
	}
}

// A certificate that cannot be kept leaves the address as it was.
func TestTheAddressStaysWhenItsCertificateCannotBeKept(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	certificateOf(t, a.do("POST", "/api/appliance/certificate/self-signed", ""))
	before := c.fingerprint()
	c.files = renameFails{appliance.InterfaceCertificateFile}
	if got := a.do("PUT", "/api/appliance/certificate/address", `{"name":"`+movedHost+`"}`); got.Code != http.StatusInternalServerError {
		t.Fatalf("got %d %s", got.Code, got.Body)
	}
	c.files = nil
	if h.server.PublicAddress() != "https://"+certificateHost || c.fingerprint() != before {
		t.Errorf("after a failed change: %s, %s", h.server.PublicAddress(), c.fingerprint())
	}
	if _, err := os.Stat(filepath.Join(c.dir, appliance.PublicURLFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the address file stayed: %v", err)
	}
}

// renameFails fails putting one file in place.
type renameFails struct{ name string }

func (renameFails) CreateTemp(dir, pattern string) (certificateFile, error) { //nolint:ireturn // the interface's own seam
	return osFiles{}.CreateTemp(dir, pattern)
}

func (f renameFails) Rename(oldPath, newPath string) error {
	if filepath.Base(newPath) == f.name {
		return errors.New("read-only file system")
	}
	return osFiles{}.Rename(oldPath, newPath)
}

func (renameFails) Remove(name string) error { return osFiles{}.Remove(name) }
