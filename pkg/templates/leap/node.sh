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

# guest-exec, should the agent refuse it, and the policy that has what it starts run as from a root
# shell rather than confined to the agent's own domain.
if [ -f /etc/sysconfig/qemu-ga ]; then
    sed -i '/^FILTER_RPC_ARGS=/s/--allow-rpcs=/--allow-rpcs=guest-exec,guest-exec-status,/' /etc/sysconfig/qemu-ga
fi
install -D -m 0644 "${files}/node/kubehenge-qga.cil" /usr/share/kubehenge/kubehenge-qga.cil
semodule -i /usr/share/kubehenge/kubehenge-qga.cil

# A node takes its updates as a new build of its template, which its cluster rolls out, and never
# reboots on its own.
systemctl mask transactional-update.timer rebootmgr.service

if [ "${distribution}" = kubeadm ]; then
    sh "${files}/node/kubeadm.sh"
fi
# The bake's files came into the snapshot with its /etc.
rm -rf "${files}"
