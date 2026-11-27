#!/bin/sh
# Prints the newest pve-manager version of the Proxmox VE repository test/e2e/pve installs from:
#
#   scripts/pve-version.sh [<hash of test/e2e/pve>]
#
# Output is version=<version>. With the hash it adds prefix=e2e-pve-<hash>-<version>-, the start of
# the cache key of Proxmox VE's image, so that a new release misses every older entry and the image
# is built again. It is the one place that composes the key. A repository that cannot be read, or
# that lists no pve-manager, is an error, never a key without a version.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
# Where test/e2e/pve/Dockerfile gets the packages from, as the index of the suite and component.
index=https://download.proxmox.com/debian/pve/dists/trixie/pve-no-subscription/binary-amd64/Packages.gz

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

"${here}/fetch.sh" "${index}" "${work}/Packages.gz" || {
    echo "pve-version: could not read ${index}" >&2
    exit 1
}
gzip -dc "${work}/Packages.gz" > "${work}/Packages" || {
    echo "pve-version: ${index} is not a gzip file" >&2
    exit 1
}
# The index keeps every version it has published. sort -V orders them as Debian does, a pre-release
# (9.0.0~10) before its release.
version=$(awk 'BEGIN { RS = ""; FS = "\n" }
    {
        name = ""; version = ""
        for (i = 1; i <= NF; i++) {
            if ($i ~ /^Package: /) name = substr($i, 10)
            if ($i ~ /^Version: /) version = substr($i, 10)
        }
        if (name == "pve-manager" && version != "") print version
    }' "${work}/Packages" | sort -V | tail -n 1)
if [ -z "${version}" ]; then
    echo "pve-version: ${index} lists no pve-manager" >&2
    exit 1
fi
echo "version=${version}"
if [ -n "${1:-}" ]; then
    echo "prefix=e2e-pve-${1}-${version}-"
fi
