package templates

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
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
		Build:  "10.2-20261109.0",
		SHA256: "6418337094ffc31874f775f5da1aca8989d29ad66cfe3824ab0047cdc5e45c09",
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
	minor, _, _ := strings.Cut(r.Build, "-")
	return s.signedImage(ctx, &pinnedImage{
		signer: signingKey{name: "AlmaLinux OS 10", armoured: almaLinux10Key, fingerprint: AlmaLinux10KeyFingerprint},
		dirs: []string{
			"https://repo.almalinux.org/almalinux/" + minor + "/cloud/x86_64_v2/images",
			"https://vault.almalinux.org/" + minor + "/cloud/x86_64_v2/images",
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
	distro: "almalinux10", releasever: "10", repos: []string{"baseos", "appstream"}, reposPackage: "almalinux-repos",
	// EPEL's own containerd needs x86-64-v3. AlmaLinux rebuilds it for v2, signed with a key of its own.
	containerd: containerdRepository("AlmaLinux EPEL 10 for x86-64-v2",
		"https://epel.repo.almalinux.org/$releasever${releasever_minor:+z}/x86_64_v2/",
		&RepositoryKey{
			Name: "almalinux-epel", URL: "https://repo.almalinux.org/almalinux/RPM-GPG-KEY-AlmaLinux-10-EPEL-AltArch",
			Fingerprints: []string{AlmaLinuxEPELAltArchKeyFingerprint},
		}),
}

func (almaLinux) Keys(_ *Release, b *Bake) []RepositoryKey {
	return keysOf(almaLinux10EL.repositories(b))
}

func (almaLinux) Recipe(_ *Release, b *Bake, keys map[string]string) (Recipe, error) {
	return almaLinux10EL.recipe(b, keys)
}
