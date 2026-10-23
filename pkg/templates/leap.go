package templates

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
)

const leapName = "leap"

// OpenSUSEKeyFingerprint is the openSUSE Project Signing Key, which signs Leap's packages and the
// checksums of its images.
const OpenSUSEKeyFingerprint = "AD485664E901B867051AB15F35A2F86E29B700A4"

//go:embed keys/opensuse.asc
var openSUSEKey []byte

// leap is openSUSE Leap in its immutable mode: a read-only root that transactional-update changes
// only as a new snapshot, booted into once complete. The bake installs what a node needs as one
// such snapshot and reboots into it.
type leap struct{}

func (leap) Name() string  { return leapName }
func (leap) Title() string { return "openSUSE Leap" }
func (leap) MAC() MAC      { return SELinux }

func (leap) Releases() []Release {
	return []Release{{
		Name: "16.1",
		// The build of the Leap 16.1 Immutable Base image, named by its file on
		// download.opensuse.org, once openSUSE releases it.
		Build:      "",
		Mode:       Transactional,
		Unreleased: "openSUSE has not released Leap 16.1 yet, and Kubehenge builds no release candidates",
	}}
}

// Image is the Immutable Base image in qcow2, with its own signed checksum file beside it.
func (leap) Image(ctx context.Context, s *Sources, r Release) (Resolved, error) {
	if r.Build == "" {
		return Resolved{}, errors.New("no build of openSUSE Leap " + r.Name + " is named yet")
	}
	dir := "https://download.opensuse.org/distribution/leap/" + r.Name + "/appliances"
	file := fmt.Sprintf("Leap-%s-Immutable.x86_64-Base-qcow-%s.qcow2", r.Name, r.Build)
	key := signingKey{name: "openSUSE", armoured: openSUSEKey, fingerprint: OpenSUSEKeyFingerprint}
	return s.signedImage(ctx, key, dir+"/"+file, dir+"/"+file+".sha256", dir+"/"+file+".sha256.asc", "leap-"+r.Name+"-"+r.Build)
}

func (leap) Keys(_ Release, b Bake) []RepositoryKey { return keysOf(leapRepositories(b)) }

func (leap) Recipe(_ Release, b Bake, keys map[string]string) (Recipe, error) {
	return leapRecipe(b, keys)
}
