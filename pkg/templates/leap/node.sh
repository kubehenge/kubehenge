#!/bin/sh
# Makes the family's immutable image a node of one distribution, inside the new snapshot of its
# root that transactional-update runs it in: the packages a node needs that the image lacks, from
# the family's and the distributions' signed repositories, and what Kubehenge and Cluster API reach
# the node with. The image brings cloud-init, the QEMU guest agent, NetworkManager and SELinux.
#
#   node.sh <distribution> [<Kubernetes version>]
set -eu

files=$(dirname "$(dirname "$0")")
distribution=$1
kubernetes=${2:-}

# Kubehenge checked each key's fingerprint before the bake; imported, zypper trusts it without
# asking.
for key in "${files}"/keys/*.asc; do
    install -D -m 0644 "${key}" "/etc/pki/rpm-gpg/kubehenge-$(basename "${key}")"
    rpm --import "${key}"
done
for repo in "${files}"/repos/*.repo; do
    install -m 0644 "${repo}" "/etc/zypp/repos.d/kubehenge-$(basename "${repo}")"
done

packages="container-selinux curl gawk tar"
case "${distribution}" in
    kubeadm)
        version=${kubernetes#v}
        packages="${packages} containerd runc kubelet=${version} kubeadm=${version} kubectl=${version} cri-tools"
        ;;
    rke2) packages="${packages} rke2-selinux" ;;
    k3s) packages="${packages} k3s-selinux" ;;
    *)
        echo "node.sh: no such distribution: ${distribution}" >&2
        exit 1
        ;;
esac
# shellcheck disable=SC2086 # one word per package
zypper --non-interactive install --no-recommends ${packages}
rm -f /etc/zypp/repos.d/kubehenge-*.repo /etc/pki/rpm-gpg/kubehenge-*.asc
zypper --non-interactive clean --all

# guest-exec and the reading of files, should the agent refuse them, and the policy that has what
# guest-exec starts run as from a root shell rather than confined to the agent's own domain, and
# lets the agent read the boot log (node/boot-log.sh) and cloud-init's output.
if [ -f /etc/sysconfig/qemu-ga ]; then
    sed -i '/^FILTER_RPC_ARGS=/s/--allow-rpcs=/--allow-rpcs=guest-exec,guest-exec-status,guest-file-open,guest-file-read,guest-file-close,/' /etc/sysconfig/qemu-ga
fi
install -D -m 0644 "${files}/node/kubehenge-qga.cil" /usr/share/kubehenge/kubehenge-qga.cil
semodule -i /usr/share/kubehenge/kubehenge-qga.cil
install -D -m 0755 "${files}/node/boot-log.sh" /usr/libexec/kubehenge/boot-log
install -m 0644 "${files}/node/kubehenge-boot-log.service" "${files}/node/kubehenge-boot-log.timer" /usr/lib/systemd/system/

# A clone's first boot, with its new machine ID, applies the presets again, which enable only the
# services they name.
mkdir -p /usr/lib/systemd/system-preset
printf 'enable qemu-guest-agent.service\nenable kubehenge-boot-log.timer\n' > /usr/lib/systemd/system-preset/80-kubehenge.preset
systemctl preset qemu-guest-agent kubehenge-boot-log.timer

# A node takes its updates as a new build of its template, which its cluster rolls out, and never
# reboots on its own.
systemctl mask transactional-update.timer rebootmgr.service

# The CIS benchmarks ask for the journal on disk, and kube-bench reads it there.
mkdir -p /usr/lib/systemd/journald.conf.d
printf '[Journal]\nStorage=persistent\n' > /usr/lib/systemd/journald.conf.d/50-kubehenge.conf

if [ "${distribution}" = kubeadm ]; then
    sh "${files}/node/kubeadm.sh"
fi
# The bake's files came into the snapshot with its /etc.
rm -rf "${files}"
