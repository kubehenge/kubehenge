#!/bin/sh
# Makes the family's minimal bootable image a node of one distribution, as one layer of its build:
# the packages a node needs, from the family's and the distributions' signed repositories, and what
# Kubehenge and Cluster API reach the node with. The bake's files are mounted beside it.
#
#   node.sh <distribution> [<Kubernetes version>]
set -eu

files=$(dirname "$(dirname "$0")")
distribution=$1
kubernetes=${2:-}

# Each repository file names its key under /etc/pki/rpm-gpg; neither stays in the image, whose
# packages only a new build changes.
for repo in "${files}"/repos/*.repo; do
    install -m 0644 "${repo}" "/etc/yum.repos.d/kubehenge-$(basename "${repo}")"
done
for key in "${files}"/keys/*.asc; do
    install -m 0644 "${key}" "/etc/pki/rpm-gpg/kubehenge-$(basename "${key}")"
done

# curl, awk and tar for the installs of RKE2, k3s and kubeadm's images; NetworkManager, the only
# network renderer cloud-init has on EL; br_netfilter, which every distribution's pod network
# needs, is only in the kernel's extra modules, which must be of the image's own kernel.
kernel=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}\n' kernel-core | tail -n 1)
packages="cloud-init qemu-guest-agent NetworkManager container-selinux curl gawk tar kernel-modules-extra-${kernel}"
case "${distribution}" in
    kubeadm)
        version=${kubernetes#v}
        packages="${packages} containerd runc kubelet-${version} kubeadm-${version} kubectl-${version} cri-tools iptables-nft"
        ;;
    # The portmap plugin, which Calico's and Canal's host ports go through, runs iptables from the
    # node's own PATH; k3s brings its own.
    rke2) packages="${packages} rke2-selinux iptables-nft" ;;
    k3s) packages="${packages} k3s-selinux" ;;
    *)
        echo "node.sh: no such distribution: ${distribution}" >&2
        exit 1
        ;;
esac
# A slow mirror is left for the next, as in build.sh.
# shellcheck disable=SC2086 # one word per package
dnf -y install --setopt=install_weak_deps=False --setopt=minrate=100k ${packages}
dnf clean all
rm -f /etc/yum.repos.d/kubehenge-*.repo /etc/pki/rpm-gpg/kubehenge-*.asc
rm -rf /var/cache/* /var/log/* /var/lib/dnf

# guest-exec, which the family's agent refuses by default, and the policy that has what it starts
# run as from a root shell rather than confined to the agent's own domain.
sed -i '/^FILTER_RPC_ARGS=/s/--allow-rpcs=/--allow-rpcs=guest-exec,guest-exec-status,/' /etc/sysconfig/qemu-ga
grep -q -- '--allow-rpcs=guest-exec,guest-exec-status,' /etc/sysconfig/qemu-ga
install -D -m 0644 "${files}/node/kubehenge-qga.cil" /usr/share/kubehenge/kubehenge-qga.cil
semodule -i /usr/share/kubehenge/kubehenge-qga.cil
install -D -m 0644 "${files}/node/kubehenge-calico.cil" /usr/share/kubehenge/kubehenge-calico.cil
semodule -i /usr/share/kubehenge/kubehenge-calico.cil
printf 'd /var/lib/calico 0755 root root -\n' > /usr/lib/tmpfiles.d/kubehenge-calico.conf
install -D -m 0644 "${files}/node/cloud.cfg" /etc/cloud/cloud.cfg.d/90-kubehenge.cfg

mkdir -p /usr/lib/bootc/install /usr/lib/bootc/kargs.d
printf '[install]\nroot-fs-type = "xfs"\n' > /usr/lib/bootc/install/20-kubehenge.toml
# Proxmox VE's console for Kubehenge's VMs is their serial port.
printf 'kargs = ["console=tty0", "console=ttyS0,115200n8"]\n' > /usr/lib/bootc/kargs.d/10-kubehenge.toml

# A clone's first boot, with its new machine ID, applies the presets again, and the family's end in
# disabling every service they do not name.
mkdir -p /usr/lib/systemd/system-preset
printf 'enable qemu-guest-agent.service\n' > /usr/lib/systemd/system-preset/80-kubehenge.preset
systemctl preset qemu-guest-agent
# A node takes its updates as a new build of its template, which its cluster rolls out; fwupd
# cannot go, since shim needs it.
systemctl mask bootc-fetch-apply-updates.timer fwupd-refresh.timer

# The image has no /var/log/journal, so the journal would stay in memory and go with each reboot;
# the CIS benchmarks ask for it on disk, and kube-bench reads it there.
mkdir -p /usr/lib/systemd/journald.conf.d
printf '[Journal]\nStorage=persistent\n' > /usr/lib/systemd/journald.conf.d/50-kubehenge.conf

if [ "${distribution}" = rke2 ]; then
    # /opt is in /var on RKE2's images (elimage.go), where RKE2 would make the CNI plugins'
    # directory itself, with /var/opt's label. Made at every boot before RKE2 starts, it is labelled
    # for the containers that install the plugins, and what they install there takes its label.
    install -D -m 0644 "${files}/node/kubehenge-cni.cil" /usr/share/kubehenge/kubehenge-cni.cil
    semodule -i /usr/share/kubehenge/kubehenge-cni.cil
    printf 'd /var/opt/cni/bin 0755 root root -\n' > /usr/lib/tmpfiles.d/kubehenge-cni.conf
    # The image keeps its system users in /usr/lib for nss-altfiles, an etcd user among them, while
    # RKE2 looks up the one its CIS profile runs etcd as in /etc alone.
    for db in passwd group; do
        if grep -q '^etcd:' "/usr/lib/${db}"; then
            grep '^etcd:' "/usr/lib/${db}" >> "/etc/${db}"
            sed -i '/^etcd:/d' "/usr/lib/${db}"
        fi
    done
fi

if [ "${distribution}" = kubeadm ]; then
    # kubeadm's packages install the CNI plugins into /opt, which stays in the read-only image; the
    # cluster's CNI installs its own beside them, in an overlay that ostree keeps in /var.
    printf 'enable ostree-state-overlay@.service opt\n' >> /usr/lib/systemd/system-preset/80-kubehenge.preset
    systemctl enable ostree-state-overlay@opt.service
    sh "${files}/node/kubeadm.sh"
fi
