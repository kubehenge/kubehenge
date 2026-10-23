package templates

import (
	"fmt"
	"strings"
)

// The disks and memory of an image-mode bake: the template's disk, which clones only grow, and the
// builder's, which holds the family's image, the composed root file system and its two images.
const (
	imageDiskGiB    = 12
	imageBuilderGiB = 20
	imageBuilderMiB = 4096
)

// imageModeFilesDir holds el/build.sh and what it builds with.
const imageModeFilesDir = "el"

// elFamily is what an Enterprise Linux family in image mode brings to its bake.
type elFamily struct {
	// distro, releasever, repos and reposPackage compose the minimal root file system: the
	// manifests' name for the family, its major release, the ids of the repositories its packages
	// come from and the package that defines them.
	distro       string
	releasever   string
	repos        []string
	reposPackage string
	// containerd is where a kubeadm node's containerd and runc come from, built for the family's
	// CPU level; the family itself has neither.
	containerd *repository
}

var (
	rke2SELinux = &repository{
		id: "rke2-selinux", name: "RKE2's SELinux policy", baseURL: "https://rpm.rancher.io/rke2/stable/common/centos/10/noarch",
		key: rancherKey, signedMetadata: true,
	}
	// k3s publishes its policy for EL 10 in its testing channel alone; its stable channel has
	// only EL 9's, in a repository whose metadata is not signed.
	k3sSELinux = &repository{
		id: "k3s-selinux", name: "k3s's SELinux policy", baseURL: "https://rpm-testing.rancher.io/k3s/testing/common/centos/10/noarch",
		key: rancherTestingKey, signedMetadata: true,
	}
)

// repositories are those a bake of the distribution adds to the family's.
func (f *elFamily) repositories(b Bake) []*repository {
	switch b.Distribution {
	case RKE2:
		return []*repository{rke2SELinux}
	case K3s:
		return []*repository{k3sSELinux}
	case Kubeadm:
		return []*repository{kubernetesRepository(b.KubernetesVersion), f.containerd}
	case Talos:
	}
	return nil
}

// manifest is what bootc-base-imagectl composes the minimal root file system from: upstream's
// minimal manifest, with what grows the root file system to a clone's disk.
func (f *elFamily) manifest(d Distribution) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "releasever: %s\nrepos:\n", f.releasever)
	for _, r := range f.repos {
		fmt.Fprintf(&sb, "  - %s\n", r)
	}
	fmt.Fprintf(&sb, "variables:\n  distro: %q\npackages:\n  - %s\n  - cloud-utils-growpart\n", f.distro, f.reposPackage)
	if d == RKE2 || d == K3s {
		// They install themselves into /usr/local when a node boots, which the minimal manifest
		// keeps in the read-only /usr; in /var the policy labels it as it would /usr/local. kubeadm's
		// nodes keep it in /usr, where the image's packages, the CNI plugins in /opt among them, can
		// install into it.
		sb.WriteString("opt-usrlocal: \"var\"\n")
	}
	sb.WriteString("include:\n  - minimal/manifest.yaml\n  - standard/generic-growfs.yaml\n")
	return sb.String()
}

// recipe builds the node image in a VM of the family's cloud image and installs it onto the
// template's disk (el/build.sh), then boots it as a clone would be.
func (f *elFamily) recipe(b Bake, keys map[string]string) (Recipe, error) {
	repos := f.repositories(b)
	if repos == nil {
		return Recipe{}, fmt.Errorf("no node image of %s", b.Distribution)
	}
	files, build, err := recipeBuild(b, repos, keys, imageModeFilesDir)
	if err != nil {
		return Recipe{}, err
	}
	files[imageModeFilesDir+"/manifest.yaml"] = f.manifest(b.Distribution)
	userData, err := bakeUserData(files, build)
	if err != nil {
		return Recipe{}, err
	}
	image := &NodeImageBoot{UserData: "#cloud-config\n", Checks: nodeChecks(b)}
	if b.Distribution == Kubeadm {
		// The images go into containerd's store in /var, which only a booted node has.
		image.UserData = fmt.Sprintf("#cloud-config\nruncmd:\n  - [%s, %s]\n", imagesPath, b.KubernetesVersion)
	}
	return Recipe{
		UserData:       userData,
		Checks:         [][]string{builtCheck},
		Image:          image,
		Cleanup:        nodeCleanup,
		DiskGiB:        imageDiskGiB,
		BuilderDiskGiB: imageBuilderGiB,
		MemoryMiB:      imageBuilderMiB,
		Log:            buildLog,
	}, nil
}
