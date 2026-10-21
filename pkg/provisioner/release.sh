#!/bin/sh
# Puts a release of RKE2 or k3s where its bootstrap provider's air-gapped install takes it from, every
# file checked first against the release's checksum file, and that file and the install script
# against the SHA-256s Kubehenge's controller recorded when a cluster first used the release:
#
#   kubehenge-release rke2|k3s <version> <checksum file's SHA-256> <install script's SHA-256> <file>...
set -eu

distribution=$1
version=$2
sums=$3
script=$4
shift 4

case "${distribution}" in
    rke2) repository=rancher/rke2 ;;
    k3s) repository=k3s-io/k3s ;;
    *)
        echo "kubehenge-release: no such distribution: ${distribution}" >&2
        exit 1
        ;;
esac
release="https://github.com/${repository}/releases/download/${version}"
images="/var/lib/rancher/${distribution}/agent/images"
# On disk: /tmp may be kept in memory, and the images take hundreds of MB.
work=$(mktemp -d -p /var/tmp)
trap 'rm -rf "${work}"' EXIT

fetch() {
    curl --fail --silent --show-error --location --proto '=https' --retry 5 --retry-all-errors --output "${work}/$2" "$1"
}

fetch "${release}/sha256sum-amd64.txt" sha256sum-amd64.txt
fetch "https://raw.githubusercontent.com/${repository}/${version}/install.sh" install.sh
printf '%s  %s\n%s  %s\n' "${sums}" sha256sum-amd64.txt "${script}" install.sh > "${work}/recorded"
for file in "$@"; do
    awk -v file="${file}" '$2 == file { print; found = 1 } END { exit !found }' "${work}/sha256sum-amd64.txt" >> "${work}/listed" || {
        echo "kubehenge-release: ${version} lists no ${file}" >&2
        exit 1
    }
    fetch "${release}/${file}" "${file}"
done
(cd "${work}" && sha256sum --check --strict --quiet recorded && sha256sum --check --strict --quiet listed)

install -D -m 0755 "${work}/install.sh" /opt/install.sh
mkdir -p "${images}"
for file in "$@"; do
    case "${file}" in
        *images*) mv "${work}/${file}" "${images}/" ;;
        k3s) install -m 0755 "${work}/k3s" /usr/local/bin/k3s ;;
        *) install -D -m 0644 "${work}/${file}" "/opt/${distribution}-artifacts/${file}" ;;
    esac
done
if [ "${distribution}" = rke2 ]; then
    install -m 0644 "${work}/sha256sum-amd64.txt" /opt/rke2-artifacts/sha256sum-amd64.txt
fi
