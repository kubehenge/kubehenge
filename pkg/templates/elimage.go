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
	// distro and reposPackage compose the minimal root file system with elComposeRepos: the
	// manifests' name for the family and the package that defines its repositories.
	distro       string
	reposPackage string
	// enabled are the repositories the family's files enable. dnf fails on any of them without a
	// working mirror, so the vault stands in for each.
	enabled []elRepository
	// mirror and vault hold a folder for each minor release, named by it. The family's own server
	// keeps a minor release while it is the newest. The vault keeps it from then on.
	mirror, vault string
	// containerd is where a kubeadm node's containerd and runc come from for a minor release,
	// built for the family's CPU level. The family itself has neither.
	containerd func(minor string) *repository
}

// elComposeRepos are the ids of the repositories the minimal root file system is composed from, the
// same in both families.
var elComposeRepos = []string{elBaseOS, elAppStream}

// The ids both families give their two main repositories.
const (
	elBaseOS    = "baseos"
	elAppStream = "appstream"
)

// elRepository is one of the family's repositories: its id in the family's repository files and its
// folder in a minor release's.
type elRepository struct{ id, folder string }

// elMinor is the minor release of the release's build, such as 10.2. Every package of a bake comes
// from it, so that a new minor release reaches the templates only as a new pinned build.
func elMinor(r *Release) string {
	minor, _, _ := strings.Cut(r.Build, "-")
	return minor
}

var (
	rke2SELinux = &repository{
		id: "rke2-selinux", name: "RKE2's SELinux policy", baseURL: "https://rpm.rancher.io/rke2/stable/common/centos/10/noarch",
		key: rancherKey, signedMetadata: true,
	}
	// k3s publishes its policy for EL 10 in its testing channel alone. Its stable channel has
	// only EL 9's, in a repository whose metadata is not signed.
	k3sSELinux = &repository{
		id: "k3s-selinux", name: "k3s's SELinux policy", baseURL: "https://rpm-testing.rancher.io/k3s/testing/common/centos/10/noarch",
		key: rancherTestingKey, signedMetadata: true,
	}
)

// repositories are those a bake of the distribution adds to the family's.
func (f *elFamily) repositories(r *Release, b *Bake) []*repository {
	switch b.Distribution {
	case RKE2:
		return []*repository{rke2SELinux}
	case K3s:
		return []*repository{k3sSELinux}
	case Kubeadm:
		return []*repository{kubernetesRepository(b.KubernetesVersion), f.containerd(elMinor(r))}
	case Talos:
	}
	return nil
}

// releaseFiles tell build.sh and node.sh the minor release every package comes from (el/release).
// el/vault names each repository's folder on its own server and in its vault.
func (f *elFamily) releaseFiles(files map[string]string, r *Release, repos []*repository) {
	minor := elMinor(r)
	files[imageModeFilesDir+"/release"] = minor + "\n"
	var sb strings.Builder
	for _, repo := range f.enabled {
		fmt.Fprintf(&sb, "%s %s%s/%s %s%s/%s\n", repo.id, f.mirror, minor, repo.folder, f.vault, minor, repo.folder)
	}
	for _, repo := range repos {
		if repo.vault != "" {
			fmt.Fprintf(&sb, "kubehenge-%s %s %s\n", repo.id, repo.baseURL, repo.vault)
		}
	}
	files[imageModeFilesDir+"/vault"] = sb.String()
}

// manifest is what bootc-base-imagectl composes the minimal root file system from: upstream's
// minimal manifest, with what grows the root file system to a clone's disk. The kernel's extra
// modules hold br_netfilter, which every distribution's pod network needs. Composed with the
// kernel, they come from the same mirror's metadata: a later install can reach a mirror one sync
// behind, which the metalink still accepts and which lacks a kernel published hours before. The
// release is quoted: rpm-ostree takes it as a string or an integer, and YAML reads 10.2 as a float.
func (f *elFamily) manifest(d Distribution, minor string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "releasever: %q\nrepos:\n", minor)
	for _, r := range elComposeRepos {
		fmt.Fprintf(&sb, "  - %s\n", r)
	}
	fmt.Fprintf(&sb, "variables:\n  distro: %q\npackages:\n  - %s\n  - cloud-utils-growpart\n  - kernel-modules-extra\n", f.distro, f.reposPackage)
	if d == RKE2 || d == K3s {
		// They install themselves into /usr/local when a node boots, which the minimal manifest
		// keeps in the read-only /usr. In /var the policy labels it as it would /usr/local. kubeadm's
		// nodes keep it in /usr, where the image's packages, the CNI plugins in /opt among them, can
		// install into it.
		sb.WriteString("opt-usrlocal: \"var\"\n")
	}
	sb.WriteString("include:\n  - minimal/manifest.yaml\n  - standard/generic-growfs.yaml\n")
	return sb.String()
}

// recipe builds the node image in a VM of the family's cloud image and installs it onto the
// template's disk (el/build.sh), then boots it as a clone would be.
func (f *elFamily) recipe(r *Release, b *Bake, keys map[string]string) (Recipe, error) {
	if b.Distribution == Talos {
		return f.talosRecipe(r, b, keys)
	}
	repos := f.repositories(r, b)
	if repos == nil {
		return Recipe{}, fmt.Errorf("no node image of %s", b.Distribution)
	}
	files, build, err := recipeBuild(b, repos, keys, imageModeFilesDir)
	if err != nil {
		return Recipe{}, err
	}
	f.releaseFiles(files, r, repos)
	files[imageModeFilesDir+"/manifest.yaml"] = f.manifest(b.Distribution, elMinor(r))
	userData, err := bakeUserData(files, build)
	if err != nil {
		return Recipe{}, err
	}
	// Not an empty cloud-config, which EL 10's cloud-init fails on in its first stage.
	image := &NodeImageBoot{UserData: "#cloud-config\n{}\n", Checks: nodeChecks(b)}
	if b.Distribution == Kubeadm {
		// The images go into containerd's store in /var, which only a booted node has.
		image.UserData = pullUserData(b.Proxy, b.KubernetesVersion)
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
		BootLog:        true,
	}, nil
}
