package appliance

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// TestTheNodesRootsAreTheFileTheManagementClustersTalosInstalls holds the embedded roots to their
// digest and to the Talos release the management cluster runs, so that a move of the Talos pin
// fails here until the file follows it.
func TestTheNodesRootsAreTheFileTheManagementClustersTalosInstalls(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256(talosRoots())
	if got := hex.EncodeToString(sum[:]); got != talosRootsSHA256 {
		t.Errorf("talosroots.crt.gz unpacks to a file with the SHA-256 %s, want %s", got, talosRootsSHA256)
	}
	versions, err := os.ReadFile(filepath.Join("..", "..", "management-cluster", "versions.tf"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*talos\s*=\s*"(v[^"]+)"`).FindSubmatch(versions)
	if m == nil {
		t.Fatal("management-cluster/versions.tf names no Talos release")
	}
	if pinned := string(m[1]); pinned != talosRootsRelease {
		t.Errorf("management-cluster/versions.tf pins Talos %s, and talosroots.crt.gz holds the roots of %s. "+
			"Take the cacert-<date>.pem that siderolabs/tools pins for %s: Talos's Makefile names TOOLS, and that tools commit's Pkgfile "+
			"names ca_certificates_version and ca_certificates_sha256. Store it with gzip -9 -n, then set talosRootsRelease and talosRootsSHA256.", pinned, talosRootsRelease, pinned)
	}
	if n := len(nodeRootCerts()); n < 100 {
		t.Errorf("Go reads %d roots of talosroots.crt.gz", n)
	}
}

// TestAChainUnderARootTalosShipsPasses passes a host whose chain ends at a root of the nodes' file,
// among them the roots NSS's own policy leaves out and Talos still trusts.
func TestAChainUnderARootTalosShipsPasses(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"DigiCert Trusted Root G4", "Buypass Class 2 Root CA", "Buypass Class 3 Root CA", "QuoVadis Root CA 1 G3",
		"QuoVadis Root CA 3 G3", "Certigna Root CA", "TUBITAK Kamu SM SSL Kok Sertifikasi - Surum 1", "ISRG Root X1",
	} {
		i := slices.IndexFunc(nodeRootCerts(), func(c *x509.Certificate) bool { return c.Subject.CommonName == name })
		if i < 0 {
			t.Errorf("the nodes' roots lack %q", name)
			continue
		}
		a := &Appliance{SiteChains: fakeChains(map[string][]*x509.Certificate{"ghcr.io": {nodeRootCerts()[i]}})}
		if got := a.CheckSite(t.Context(), &Settings{SiteCA: newTestRoot(t, "Internal-R2").pem()}); len(got.Errors) != 0 || len(got.Warnings) != 0 {
			t.Errorf("a chain under %q: %+v", name, got)
		}
	}
}

// TestPublicNameKnowsTheNodesRootsByTheirCommonName reads a name as Go's verifier gives a candidate
// authority in its error.
func TestPublicNameKnowsTheNodesRootsByTheirCommonName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{"ISRG Root X1": true, "DigiCert Trusted Root G4": true, "Corp Inspection CA": false, "": false} {
		if got := publicName(name); got != want {
			t.Errorf("publicName(%q) = %v", name, got)
		}
	}
}
