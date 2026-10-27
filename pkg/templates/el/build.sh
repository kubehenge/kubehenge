#!/bin/sh
# Builds a node image in the bake's builder, a VM of the family's own cloud image, and installs it
# onto the template's disk: a minimal root file system composed from the family's signed
# repositories, the node's layer on top of it (node.sh), checked by bootc and installed by bootc
# itself. It works in the directory holding the bake's files, the parent of its own:
#
#   build.sh <distribution> [<Kubernetes version>]
set -eu

# bootc-base-imagectl and the manifests it composes from are packaged by neither AlmaLinux nor
# Rocky Linux; both take them from CentOS Stream's bootc image, as AlmaLinux's own image builds do.
# That image is only unpacked here, never run: it needs x86-64-v3.
tools=quay.io/centos-bootc/centos-bootc:stream10@sha256:ec1bd8cb6479d1f2f61c85372afce319b932b6b1f671648f0887e9da01e48272
target=/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0

distribution=$1
kubernetes=${2:-}
cd "$(dirname "$0")/.."

# dnf and rpm-ostree stay on one mirror from the mirror list until it serves under 1000 bytes/s, and
# one serving 50 kB/s keeps a bake past its timeout. Under 100 kB/s they move to the next mirror; a
# bake that slow on average would not finish in time anyway. node.sh does the same.
sed -i '/^\[/a minrate=100k' /etc/yum.repos.d/*.repo

dnf -y install podman rpm-ostree bootc ostree skopeo

container=$(podman create "${tools}")
mounted=$(podman mount "${container}")
# Without the container's labels, which only an unconfined caller could run them with.
cp -R --preserve=mode,ownership,timestamps "${mounted}/usr/libexec/bootc-base-imagectl" /usr/libexec/
cp -R --preserve=mode,ownership,timestamps "${mounted}/usr/share/doc/bootc-base-imagectl" /usr/share/doc/
restorecon -R /usr/libexec/bootc-base-imagectl /usr/share/doc/bootc-base-imagectl
podman umount "${container}"
podman rm "${container}"
podman rmi "${tools}"

# Composed from the builder's own repositories and keys, which are the family's.
cp el/manifest.yaml /usr/share/doc/bootc-base-imagectl/manifests/kubehenge-node.yaml
/usr/libexec/bootc-base-imagectl build-rootfs --manifest=kubehenge-node "${PWD}/rootfs"
podman build --file el/base.Containerfile --tag localhost/kubehenge-base:build .
rm -rf rootfs

podman build --file el/Containerfile --build-arg "DISTRIBUTION=${distribution}" --build-arg "KUBERNETES=${kubernetes}" \
    --tag localhost/kubehenge-node:build .

podman run --rm --privileged --pid=host --volume /dev:/dev --volume /var/lib/containers:/var/lib/containers \
    --security-opt label=type:unconfined_t localhost/kubehenge-node:build \
    bootc install to-disk --wipe --generic-image "${target}"
touch built
