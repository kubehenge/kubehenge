package templates

import (
	"fmt"
)

const (
	// talosUnprivilegedMinor is the first Talos minor version whose imager needs no loop device, so
	// no privilege and no /dev. Earlier imagers attach one (losetup.Attach in pkg/imager/out.go),
	// which only the privileged run inside the builder VM, thrown away after the bake, allows.
	talosUnprivilegedMinor = 13
	// talosDiskGiB holds the imager's raw image, 4247 MiB from v1.11 and 2247 MiB for v1.10, and
	// stays far below the 20 GiB a Talos machine's disk is at least, as a clone of the template
	// must never be larger than a machine's disk.
	talosDiskGiB = 5
	// talosRawDiskSize is Sidero's MinRAWDiskSize. The imager adds its own partitions to it, so the
	// raw images have the sizes of the Image Factory's.
	talosRawDiskSize = 1306525696
)

// talosProfile is the imager profile of a release's nocloud image: the pinned installer base and
// the QEMU guest agent, written to a raw disk with no bootloader chosen, so that the imager picks
// the dual boot of the Image Factory's own images. It holds no kernel, which the imager's image has.
func talosProfile(release string, pin TalosPin) string {
	return fmt.Sprintf(`arch: amd64
platform: nocloud
secureboot: false
version: %s
input:
  baseInstaller:
    imageRef: %s
  systemExtensions:
    - imageRef: %s
output:
  kind: image
  outFormat: raw
  imageOptions:
    diskSize: %d
    diskFormat: raw
`, release, pin.InstallerBaseRef(), pin.GuestAgentRef(), talosRawDiskSize)
}

// talosRecipe bakes a Talos release in the builder VM: the pinned imager writes the nocloud disk
// onto the template's own disk, which is never booted.
func (*elFamily) talosRecipe(b *Bake, keys map[string]string) (Recipe, error) {
	if b.Talos.Imager == "" {
		return Recipe{}, lasting(fmt.Errorf("this Kubehenge build pins no Talos %s", b.TalosVersion))
	}
	minor, err := TalosMinor(b.TalosVersion)
	if err != nil {
		return Recipe{}, lasting(err)
	}
	files, build, err := recipeBuild(b, nil, keys, imageModeFilesDir)
	if err != nil {
		return Recipe{}, err
	}
	files[imageModeFilesDir+"/talos-imager"] = b.Talos.ImagerRef() + "\n"
	files[imageModeFilesDir+"/talos-profile.yaml"] = talosProfile(b.TalosVersion, b.Talos)
	if minor < talosUnprivilegedMinor {
		files[imageModeFilesDir+"/talos-privileged"] = ""
	}
	userData, err := bakeUserData(files, build)
	if err != nil {
		return Recipe{}, err
	}
	return Recipe{
		UserData:       userData,
		Checks:         [][]string{builtCheck},
		DiskGiB:        talosDiskGiB,
		BuilderDiskGiB: imageBuilderGiB,
		MemoryMiB:      imageBuilderMiB,
		Log:            buildLog,
	}, nil
}
