package templates

import (
	"context"
	_ "embed"
	"fmt"
)

const rockyName = "rocky"

// Rocky10KeyFingerprint is Rocky Linux 10's release engineering key, which signs its packages and
// the checksums of its images.
const Rocky10KeyFingerprint = "FC226859C0860BF0DDB95B085B106C736FEDFC85"

//go:embed keys/rocky-10.asc
var rocky10Key []byte

// rocky is Rocky Linux in image mode. Rocky publishes no maintained bootable container image that
// is free, so Kubehenge composes the node image itself from Rocky's own packages, as it does
// AlmaLinux's.
type rocky struct{}

func (rocky) Name() string  { return rockyName }
func (rocky) Title() string { return "Rocky Linux" }
func (rocky) MAC() MAC      { return SELinux }

func (rocky) Releases() []Release {
	return []Release{{
		Name: "10",
		// The build of Rocky Linux 10's GenericCloud image, which the bake boots to build the node
		// image in, named by its file on dl.rockylinux.org, and that file's SHA-256.
		// renovate: datasource=custom.rocky-cloud-images depName=rocky-10
		Build:  "10.2-20260525.0",
		SHA256: "9fc9e9ff16888bb68ac39b0392e25c9c92684d50c85f1cce6ab549363bbc4b48",
		// Rocky Linux 10 is built for x86-64-v3 and stops at boot on anything less.
		CPUType: "x86-64-v3",
		Mode:    ImageMode,
		FIPS:    true,
	}}
}

// Image is the GenericCloud image, with its own signed checksum file beside it.
//
// Rocky Linux keeps a minor release's build in that minor's folder on its mirror, and in its vault
// once a newer minor release has replaced it. The folder named 10 holds the newest minor release's
// build alone.
func (rocky) Image(ctx context.Context, s *Sources, r *Release) (Resolved, error) {
	minor := elMinor(r)
	file := fmt.Sprintf("Rocky-10-GenericCloud-Base-%s.x86_64.qcow2", r.Build)
	return s.signedImage(ctx, &pinnedImage{
		signer: signingKey{name: "Rocky Linux 10", armoured: rocky10Key, fingerprint: Rocky10KeyFingerprint},
		dirs: []string{
			rocky10EL.mirror + minor + "/images/x86_64",
			rocky10EL.vault + minor + "/images/x86_64",
		},
		file:   file,
		sums:   file + ".CHECKSUM",
		sha256: r.SHA256,
		saveAs: "rocky-" + r.Build,
	})
}

// EPEL10KeyFingerprint signs EPEL 10.
const EPEL10KeyFingerprint = "7D8D15CBFC4E62688591FB2633D98517E37ED158"

var rocky10EL = &elFamily{
	distro: "rocky10", reposPackage: "rocky-repos",
	enabled: []elRepository{
		{elBaseOS, "BaseOS/x86_64/os/"},
		{elAppStream, "AppStream/x86_64/os/"},
		{"extras", "extras/x86_64/os/"},
	},
	mirror: "https://dl.rockylinux.org/pub/rocky/", vault: "https://dl.rockylinux.org/vault/rocky/",
	// EPEL moves a minor release's folder to its archive on a day of its own.
	containerd: func(minor string) *repository {
		return containerdRepository("EPEL 10",
			"https://dl.fedoraproject.org/pub/epel/"+minor+"/Everything/x86_64/",
			"https://dl.fedoraproject.org/pub/archive/epel/"+minor+"/Everything/x86_64/",
			&RepositoryKey{Name: "epel", URL: "https://dl.fedoraproject.org/pub/epel/RPM-GPG-KEY-EPEL-10", Fingerprints: []string{EPEL10KeyFingerprint}})
	},
}

func (rocky) Keys(r *Release, b *Bake) []RepositoryKey { return keysOf(rocky10EL.repositories(r, b)) }

func (rocky) Recipe(r *Release, b *Bake, keys map[string]string) (Recipe, error) {
	return rocky10EL.recipe(r, b, keys)
}
