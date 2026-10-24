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
		// image in, named by its file on dl.rockylinux.org.
		// renovate: datasource=custom.rocky-cloud-images depName=rocky-10
		Build: "10.2-20260525.0",
		// Rocky Linux 10 is built for x86-64-v3 and stops at boot on anything less.
		CPUType: "x86-64-v3",
		Mode:    ImageMode,
	}}
}

// Image is the GenericCloud image, with its own signed checksum file beside it.
func (rocky) Image(ctx context.Context, s *Sources, r Release) (Resolved, error) {
	dir := "https://dl.rockylinux.org/pub/rocky/10/images/x86_64"
	file := fmt.Sprintf("Rocky-10-GenericCloud-Base-%s.x86_64.qcow2", r.Build)
	key := signingKey{name: "Rocky Linux 10", armoured: rocky10Key, fingerprint: Rocky10KeyFingerprint}
	return s.signedImage(ctx, key, dir+"/"+file, dir+"/"+file+".CHECKSUM", dir+"/"+file+".CHECKSUM.asc", "rocky-"+r.Build)
}

// EPEL10KeyFingerprint signs EPEL 10.
const EPEL10KeyFingerprint = "7D8D15CBFC4E62688591FB2633D98517E37ED158"

var rocky10EL = &elFamily{
	distro: "rocky10", releasever: "10", repos: []string{"baseos", "appstream"}, reposPackage: "rocky-repos",
	containerd: containerdRepository("EPEL 10",
		"https://dl.fedoraproject.org/pub/epel/$releasever_major${releasever_minor:+.$releasever_minor}/Everything/$basearch/",
		&RepositoryKey{Name: "epel", URL: "https://dl.fedoraproject.org/pub/epel/RPM-GPG-KEY-EPEL-10", Fingerprints: []string{EPEL10KeyFingerprint}}),
}

func (rocky) Keys(_ Release, b Bake) []RepositoryKey { return keysOf(rocky10EL.repositories(b)) }

func (rocky) Recipe(_ Release, b Bake, keys map[string]string) (Recipe, error) {
	return rocky10EL.recipe(b, keys)
}
