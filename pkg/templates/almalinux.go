package templates

import (
	"context"
	_ "embed"
	"fmt"
)

const almaLinuxName = "almalinux"

// AlmaLinux10KeyFingerprint is the AlmaLinux OS 10 signing key, which signs AlmaLinux 10's packages
// and the checksums of its cloud images.
const AlmaLinux10KeyFingerprint = "EE6DB7B98F5BF5EDD9DA0DE5DEE5C11CC2A1E572"

//go:embed keys/almalinux-10.asc
var almaLinux10Key []byte

// almaLinux is AlmaLinux OS in image mode: the node is a bootable container image made of
// AlmaLinux's own packages.
type almaLinux struct{}

func (almaLinux) Name() string  { return almaLinuxName }
func (almaLinux) Title() string { return "AlmaLinux" }
func (almaLinux) MAC() MAC      { return SELinux }

func (almaLinux) Releases() []Release {
	return []Release{{
		Name: "10",
		// The build of AlmaLinux 10's GenericCloud image for x86-64-v2, which the bake boots to
		// build the node image in, named by its file on repo.almalinux.org, and that file's SHA-256.
		// renovate: datasource=custom.almalinux-cloud-images depName=almalinux-10
		Build:  "10.3-20261116.0",
		SHA256: "b0230172417de9da4f56b1db3f373d6b0b4d7de3fa647a131b1685ed39ade84e",
		Mode:   ImageMode,
		FIPS:   true,
	}}
}

// Image is the GenericCloud image for x86-64-v2, which AlmaLinux 10 still builds so that it runs on
// Proxmox VE's default CPU type. Its packages are those of the node image the bake composes.
//
// AlmaLinux keeps a minor release's builds in that minor's folder on its mirror, and in its vault
// once a newer minor release has replaced it. The folder named 10 holds the newest minor release's
// builds alone.
func (almaLinux) Image(ctx context.Context, s *Sources, r *Release) (Resolved, error) {
	minor := elMinor(r)
	return s.signedImage(ctx, &pinnedImage{
		signer: signingKey{name: "AlmaLinux OS 10", armoured: almaLinux10Key, fingerprint: AlmaLinux10KeyFingerprint},
		dirs: []string{
			almaLinux10EL.mirror + minor + "/cloud/x86_64_v2/images",
			almaLinux10EL.vault + minor + "/cloud/x86_64_v2/images",
		},
		file:   fmt.Sprintf("AlmaLinux-10-GenericCloud-%s.x86_64_v2.qcow2", r.Build),
		sums:   "CHECKSUM",
		sha256: r.SHA256,
		saveAs: "almalinux-" + r.Build,
	})
}

// AlmaLinuxEPELAltArchKeyFingerprint signs AlmaLinux's rebuild of EPEL for x86-64-v2.
const AlmaLinuxEPELAltArchKeyFingerprint = "68DD137B7690F4735197C4B1B620C02335D447A6"

var almaLinux10EL = &elFamily{
	distro: "almalinux10", reposPackage: "almalinux-repos",
	enabled: []elRepository{
		{elBaseOS, "BaseOS/x86_64_v2/os/"},
		{elAppStream, "AppStream/x86_64_v2/os/"},
		{"crb", "CRB/x86_64_v2/os/"},
		{"extras", "extras/x86_64_v2/os/"},
	},
	mirror: "https://repo.almalinux.org/almalinux/", vault: "https://vault.almalinux.org/",
	// EPEL's own containerd needs x86-64-v3. AlmaLinux rebuilds it for v2, signed with a key of its
	// own, and keeps every minor release's folder.
	containerd: func(minor string) *repository {
		return containerdRepository("AlmaLinux EPEL 10 for x86-64-v2", "https://epel.repo.almalinux.org/"+minor+"/x86_64_v2/", "",
			&RepositoryKey{
				Name: "almalinux-epel", URL: "https://repo.almalinux.org/almalinux/RPM-GPG-KEY-AlmaLinux-10-EPEL-AltArch",
				Fingerprints: []string{AlmaLinuxEPELAltArchKeyFingerprint},
			})
	},
}

func (almaLinux) Keys(r *Release, b *Bake) []RepositoryKey {
	return keysOf(almaLinux10EL.repositories(r, b))
}

func (almaLinux) Recipe(r *Release, b *Bake, keys map[string]string) (Recipe, error) {
	return almaLinux10EL.recipe(r, b, keys)
}
