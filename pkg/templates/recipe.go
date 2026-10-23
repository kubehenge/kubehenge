package templates

import (
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
)

// recipeFiles are what bakes put into their VMs: el/ builds an EL family's node image, leap/ makes
// a node of Leap's immutable image, and node/ is common to every family.
//
//go:embed el leap node
var recipeFiles embed.FS

const (
	// buildDir holds a bake's files in its VM, and buildLog its account of the build.
	buildDir = "/var/lib/kubehenge-build"
	buildLog = "/var/log/kubehenge-build.log"
	// imagesPath is node/images.sh in a kubeadm node.
	imagesPath = "/usr/libexec/kubehenge/images"
)

// The commands a bake runs in the guest more than once.
const (
	cloudInit = "cloud-init"
	systemctl = "systemctl"
)

// builtCheck is the first check of a bake: its build left its marker. The build failing is only a
// cloud-init error, which says nothing of why; the end of buildLog does.
var builtCheck = []string{"sh", "-c", "cloud-init status --wait > /dev/null; test -e " + buildDir + "/built"}

// repository is a package repository a bake adds while it makes the node, in the INI form dnf and
// zypper both read.
type repository struct {
	id, name, baseURL string
	// key signs its packages.
	key *RepositoryKey
	// signedMetadata says the repository signs its metadata too, which is then checked.
	signedMetadata bool
	// only limits what dnf takes from it.
	only []string
}

func (r *repository) file() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[kubehenge-%s]\nname=%s\nbaseurl=%s\nenabled=1\ngpgcheck=1\n", r.id, r.name, r.baseURL)
	if r.signedMetadata {
		sb.WriteString("repo_gpgcheck=1\n")
	}
	fmt.Fprintf(&sb, "gpgkey=file:///etc/pki/rpm-gpg/kubehenge-%s.asc\n", r.key.Name)
	if len(r.only) > 0 {
		fmt.Fprintf(&sb, "includepkgs=%s\n", strings.Join(r.only, " "))
	}
	return sb.String()
}

// keysOf are the keys of the repositories, each once.
func keysOf(repos []*repository) []RepositoryKey {
	var keys []RepositoryKey
	for _, r := range repos {
		if !slices.ContainsFunc(keys, func(k RepositoryKey) bool { return k.Name == r.key.Name }) {
			keys = append(keys, *r.key)
		}
	}
	return keys
}

// containerdRepository is where a kubeadm node's containerd and runc come from on a family that
// has neither, and nothing else.
func containerdRepository(name, baseURL string, key *RepositoryKey) *repository {
	const containerd = "containerd"
	return &repository{id: containerd, name: name + " (" + containerd + ")", baseURL: baseURL, key: key, only: []string{containerd, "runc"}}
}

// The Rancher keys sign the repositories of the SELinux policies of RKE2 and of k3s.
const (
	RancherKeyFingerprint        = "C8CFF216455126E9B9C918BE925EA29AE257814A"
	RancherTestingKeyFingerprint = "75E0BA1CD351351DFCEFCA96BE7AD0BAC850F11B"
)

var (
	rancherKey        = &RepositoryKey{Name: "rancher", URL: "https://rpm.rancher.io/public.key", Fingerprints: []string{RancherKeyFingerprint}}
	rancherTestingKey = &RepositoryKey{Name: "rancher-testing", URL: "https://rpm-testing.rancher.io/public.key", Fingerprints: []string{RancherTestingKeyFingerprint}}
)

// kubernetesRepository is pkgs.k8s.io's for the minor version's packages.
func kubernetesRepository(version string) *repository {
	const name = "kubernetes"
	series := minorSeries(version)
	base := "https://pkgs.k8s.io/core:/stable:/" + series + "/rpm/"
	key := &RepositoryKey{Name: name, URL: base + "repodata/repomd.xml.key", Fingerprints: []string{KubernetesPackagesKeyFingerprint}}
	return &repository{id: name, name: "Kubernetes " + series, baseURL: base, key: key, signedMetadata: true}
}

func minorSeries(v string) string {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	return major + "." + minor
}

// recipeBuild gathers a bake's files, the family's directory and node/ with the repositories and
// their keys, and the command that runs the family's build.sh on them.
func recipeBuild(b Bake, repos []*repository, keys map[string]string, dir string) (map[string]string, []string, error) {
	if b.Distribution == Kubeadm && !versionRE.MatchString(b.KubernetesVersion) {
		return nil, nil, fmt.Errorf("kubernetes version %q is not of the form v1.2.3", b.KubernetesVersion)
	}
	files := map[string]string{}
	for _, r := range repos {
		files["repos/"+r.id+".repo"] = r.file()
	}
	for _, k := range keysOf(repos) {
		armoured, ok := keys[k.Name]
		if !ok {
			return nil, nil, fmt.Errorf("the bake needs the %s key", k.Name)
		}
		files["keys/"+k.Name+".asc"] = armoured
	}
	for _, d := range []string{dir, "node"} {
		err := fs.WalkDir(recipeFiles, d, func(name string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			content, err := recipeFiles.ReadFile(name)
			if err != nil {
				return fmt.Errorf("reading %s: %w", name, err)
			}
			files[name] = string(content)
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("reading the bake's files: %w", err)
		}
	}
	build := []string{"sh", path.Join(buildDir, dir, "build.sh"), string(b.Distribution)}
	if b.Distribution == Kubeadm {
		build = append(build, b.KubernetesVersion)
	}
	return files, build, nil
}

// enableGuestExec is the first thing the bake's VM does, before cloud-init's other modules: the EL
// families' agent refuses guest-exec, and their policy keeps what guest-exec starts in the agent's
// own domain. The builder waits until guest-exec answers.
const enableGuestExec = `semodule -i %[1]s
if [ -f /etc/sysconfig/qemu-ga ]; then sed -i '/^FILTER_RPC_ARGS=/s/--allow-rpcs=/--allow-rpcs=guest-exec,guest-exec-status,/' /etc/sysconfig/qemu-ga; fi
systemctl try-restart qemu-guest-agent
`

// bakeUserData is the cloud-config of a bake's first boot: guest-exec enabled at once, the bake's
// files in buildDir, and its build run with its output in buildLog.
func bakeUserData(files map[string]string, build []string) (string, error) {
	cil, err := recipeFiles.ReadFile("node/kubehenge-qga.cil")
	if err != nil {
		return "", fmt.Errorf("reading the bake's files: %w", err)
	}
	const cilPath = "/run/kubehenge-qga.cil"
	var sb strings.Builder
	sb.WriteString("#cloud-config\nbootcmd:\n  - |\n")
	fmt.Fprintf(&sb, "    echo %s | base64 -d > %s\n", base64.StdEncoding.EncodeToString(cil), cilPath)
	for line := range strings.SplitSeq(strings.TrimSuffix(fmt.Sprintf(enableGuestExec, cilPath), "\n"), "\n") {
		fmt.Fprintf(&sb, "    %s\n", line)
	}
	sb.WriteString("write_files:\n")
	for _, name := range slices.Sorted(maps.Keys(files)) {
		mode := "0600"
		if path.Ext(name) == ".sh" {
			mode = "0700"
		}
		fmt.Fprintf(&sb, "  - path: %s\n    permissions: %q\n    encoding: b64\n    content: %s\n",
			path.Join(buildDir, name), mode, base64.StdEncoding.EncodeToString([]byte(files[name])))
	}
	fmt.Fprintf(&sb, "runcmd:\n  - [sh, -c, %q]\n", strings.Join(build, " ")+" > "+buildLog+" 2>&1")
	return sb.String(), nil
}

// avcDenied finds no denial of SELinux in the kernel's log or the audit log, whichever has them.
const avcDenied = `! journalctl -b -k -o cat --no-pager | grep -qF 'avc:  denied' && ! grep -qsF 'avc:  denied' /var/log/audit/audit.log`

// nodeChecks prove a node's boot as a clone's goes: cloud-init finishes without warnings, which
// Cluster API's Proxmox VE provider counts as a failed boot, the root is read-only, SELinux enforces
// and denied nothing, and the distribution's parts are in place.
func nodeChecks(b Bake) [][]string {
	checks := [][]string{
		{cloudInit, "status", "--wait"},
		{systemctl, "is-active", "qemu-guest-agent"},
		{"sh", "-c", `test "$(getenforce)" = Enforcing`},
		{"sh", "-c", "semodule -l | grep -qx kubehenge-qga"},
		{"sh", "-c", "! touch /usr/kubehenge 2> /dev/null"},
		// Every distribution's pod network bridges through it.
		{"sh", "-c", "modinfo br_netfilter > /dev/null"},
	}
	switch b.Distribution {
	case RKE2, K3s:
		checks = append(checks, []string{"sh", "-c", fmt.Sprintf("rpm -q %[1]s-selinux container-selinux > /dev/null && semodule -l | grep -qx %[1]s", b.Distribution)})
	case Kubeadm:
		checks = append(checks,
			[]string{"sh", "-c", `test "$(kubeadm version -o short)" = ` + b.KubernetesVersion},
			[]string{"sh", "-c", "grep -q 'SystemdCgroup = true' /etc/containerd/config.toml && grep -q 'enable_selinux = true' /etc/containerd/config.toml"},
			[]string{"sh", "-c", "test -d /sys/module/br_netfilter && test -d /sys/module/nf_conntrack"},
			[]string{"sh", "-c", "grep -qx '      type: spc_t' /etc/kubernetes/patches/etcd+strategic.yaml"},
			[]string{systemctl, "is-enabled", "kubelet"},
			[]string{systemctl, "is-active", "containerd"},
			// The control-plane images are in containerd, so nodes do not wait on the registry,
			// and containerd starts pods on kubeadm's pause image among them.
			[]string{"sh", "-c", "crictl images | grep -q kube-apiserver"},
			[]string{"sh", "-c", `grep -qF "$(kubeadm config images list --kubernetes-version ` + b.KubernetesVersion + ` | grep /pause:)" /etc/containerd/config.toml`},
		)
	case Talos:
	}
	return append(checks, []string{"sh", "-c", avcDenied})
}

// nodeCleanup leaves the template as a fresh first boot: clones get their own machine ID, network
// and cloud-init run, and none of the bake's journal.
var nodeCleanup = [][]string{
	{cloudInit, "clean", "--logs", "--seed", "--machine-id", "--configs", "all"},
	{"sh", "-c", "journalctl --rotate > /dev/null && journalctl --vacuum-time=1s > /dev/null"},
	{"fstrim", "--all"},
	{"sync"},
}
