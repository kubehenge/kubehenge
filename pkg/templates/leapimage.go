package templates

import "fmt"

// transactionalDiskGiB is the template's disk in a transactional bake, which runs on it: at least
// Leap's image's own.
const transactionalDiskGiB = 20

// transactionalFilesDir holds leap/build.sh and what it installs with.
const transactionalFilesDir = "leap"

var (
	// RKE2's and k3s's policies for SUSE's immutable systems. k3s's stable channel does not sign
	// its metadata; its testing channel does, with the key its EL 10 policy comes with.
	rke2SELinuxSUSE = &repository{
		id: "rke2-selinux", name: "RKE2's SELinux policy", baseURL: "https://rpm.rancher.io/rke2/stable/common/slemicro/noarch",
		key: rancherKey, signedMetadata: true,
	}
	k3sSELinuxSUSE = &repository{
		id: "k3s-selinux", name: "k3s's SELinux policy", baseURL: "https://rpm-testing.rancher.io/k3s/testing/common/slemicro/noarch",
		key: rancherTestingKey, signedMetadata: true,
	}
)

// leapRepositories are what a bake of the distribution adds to Leap's own repositories, which have
// containerd.
func leapRepositories(b Bake) []*repository {
	switch b.Distribution {
	case RKE2:
		return []*repository{rke2SELinuxSUSE}
	case K3s:
		return []*repository{k3sSELinuxSUSE}
	case Kubeadm:
		return []*repository{kubernetesRepository(b.KubernetesVersion)}
	case Talos:
	}
	return nil
}

// leapRecipe installs what a node needs as a new snapshot of the image's root (leap/build.sh) and
// reboots into it, where a kubeadm node pulls its control-plane images.
func leapRecipe(b Bake, keys map[string]string) (Recipe, error) {
	repos := leapRepositories(b)
	if repos == nil {
		return Recipe{}, fmt.Errorf("no node of %s", b.Distribution)
	}
	files, build, err := recipeBuild(b, repos, keys, transactionalFilesDir)
	if err != nil {
		return Recipe{}, err
	}
	userData, err := bakeUserData(files, build)
	if err != nil {
		return Recipe{}, err
	}
	var rebootChecks [][]string
	if b.Distribution == Kubeadm {
		rebootChecks = append(rebootChecks, []string{imagesPath, b.KubernetesVersion})
	}
	return Recipe{
		UserData:     userData,
		Checks:       [][]string{builtCheck},
		Reboot:       true,
		RebootChecks: append(rebootChecks, nodeChecks(b)...),
		// Leap's image has an SSH server, whose host keys the bake's boot made.
		Cleanup: append([][]string{{"sh", "-c", "rm -f /etc/ssh/ssh_host_*"}}, nodeCleanup...),
		DiskGiB: transactionalDiskGiB,
		Log:     buildLog,
	}, nil
}
