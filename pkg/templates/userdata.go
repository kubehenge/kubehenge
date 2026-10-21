package templates

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// The commands a bake runs in the guest more than once.
const (
	cloudInit = "cloud-init"
	systemctl = "systemctl"
)

//go:embed images.sh
var imagesScript string

const imagesScriptPath = "/usr/local/sbin/kubehenge-images"

// BakeUserData renders the cloud-config that prepares a cloud image to become a template. Every
// template gets the latest updates and the QEMU guest agent; a kubeadm template also gets
// containerd, the kubelet and kubeadm as Kubernetes' own installation guide sets them up, with the
// control-plane images pulled ahead, each checked against its signature (images.sh), and the
// kubelet's unit files readable by root alone, as the CIS benchmark asks. packagesKey is the
// ASCII-armoured pkgs.k8s.io key, needed for kubeadm.
func BakeUserData(b Bake, packagesKey string) (string, error) {
	var sb strings.Builder
	sb.WriteString("#cloud-config\npackage_update: true\npackage_upgrade: true\n")

	if b.KubernetesVersion == "" {
		sb.WriteString("packages:\n  - qemu-guest-agent\n")
		sb.WriteString("runcmd:\n  - [systemctl, enable, --now, qemu-guest-agent]\n")
		return sb.String(), nil
	}

	if packagesKey == "" {
		return "", errors.New("a kubeadm template needs the pkgs.k8s.io signing key")
	}
	series := minorSeries(b.KubernetesVersion)
	pkg := strings.TrimPrefix(b.KubernetesVersion, "v") + "-1.1"

	fmt.Fprintf(&sb, "apt:\n  sources:\n    kubernetes.list:\n      source: \"deb [signed-by=$KEY_FILE] https://pkgs.k8s.io/core:/stable:/%s/deb/ /\"\n      key: |\n", series)
	for line := range strings.SplitSeq(strings.TrimSpace(packagesKey), "\n") {
		sb.WriteString("        ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, `packages:
  - qemu-guest-agent
  - containerd
  - [kubelet, %[1]s]
  - [kubeadm, %[1]s]
  - [kubectl, %[1]s]
  - cri-tools
write_files:
  - path: /etc/crictl.yaml
    content: |
      runtime-endpoint: unix:///run/containerd/containerd.sock
      image-endpoint: unix:///run/containerd/containerd.sock
  - path: /etc/modules-load.d/kubernetes.conf
    content: |
      overlay
      br_netfilter
  - path: /etc/sysctl.d/90-kubernetes.conf
    content: |
      net.ipv4.ip_forward = 1
      net.bridge.bridge-nf-call-iptables = 1
      net.bridge.bridge-nf-call-ip6tables = 1
  - path: %[3]s
    permissions: "0700"
    encoding: b64
    content: %[4]s
runcmd:
  - [systemctl, enable, --now, qemu-guest-agent]
  - [modprobe, overlay]
  - [modprobe, br_netfilter]
  - [sysctl, --system]
  - [sh, -c, "mkdir -p /etc/containerd && containerd config default | sed 's/SystemdCgroup = false/SystemdCgroup = true/' > /etc/containerd/config.toml"]
  - [systemctl, restart, containerd]
  - [apt-mark, hold, kubelet, kubeadm, kubectl]
  - [systemctl, enable, kubelet]
  - [%[3]s, %[2]s]
`, pkg, b.KubernetesVersion, imagesScriptPath, base64.StdEncoding.EncodeToString([]byte(imagesScript)))
	return sb.String(), nil
}

// BakeChecks are run through the guest agent once cloud-init has finished; each must exit 0.
func BakeChecks(b Bake) [][]string {
	checks := [][]string{
		// Exit 2 means warnings, which Cluster API's Proxmox VE provider treats as a failed boot.
		{cloudInit, "status", "--wait"},
		{systemctl, "is-active", "qemu-guest-agent"},
	}
	if b.KubernetesVersion != "" {
		checks = append(checks,
			[]string{"sh", "-c", `test "$(kubeadm version -o short)" = ` + b.KubernetesVersion},
			[]string{"sh", "-c", "grep -q 'SystemdCgroup = true' /etc/containerd/config.toml"},
			[]string{systemctl, "is-enabled", "kubelet"},
			[]string{systemctl, "is-active", "containerd"},
			// The control-plane images are in containerd, so nodes do not wait on the registry,
			// and containerd starts pods on kubeadm's pause image among them.
			[]string{"sh", "-c", "crictl images | grep -q kube-apiserver"},
			[]string{"sh", "-c", `grep -qF "$(kubeadm config images list --kubernetes-version ` + b.KubernetesVersion + ` | grep /pause:)" /etc/containerd/config.toml`},
		)
	}
	return checks
}

// BakeCleanup leaves the image as a fresh first boot: clones get their own machine ID, SSH host
// keys and cloud-init run.
var BakeCleanup = [][]string{
	{"apt-get", "clean"},
	{"sh", "-c", "rm -f /etc/ssh/ssh_host_*"},
	{cloudInit, "clean", "--logs", "--seed", "--machine-id", "--configs", "all"},
	{"fstrim", "--all"},
	{"sync"},
}

func minorSeries(v string) string {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	return major + "." + minor
}
