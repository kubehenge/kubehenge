#!/bin/sh
# Writes a Talos release's nocloud disk onto the template's disk with Sidero's own imager, pinned by
# digest (talos-imager beside this script), from the profile the bake wrote beside it
# (talos-profile.yaml). The bake's files are in the directory above this script's, where it works:
#
#   talos.sh <target disk>
set -eu

target=$1
here=$(cd "$(dirname "$0")" && pwd)
cd "${here}/.."

imager=$(cat "${here}/talos-imager")
out=${PWD}/talos-out
certs=${PWD}/talos-ca
rm -rf "${out}" "${certs}"
mkdir "${out}"

# The imager pulls the installer base and its extensions itself and trusts only what it is given:
# Go reads SSL_CERT_DIR alone then, so a copy the container cannot read fails every pull instead of
# falling back to the image's own roots. A copy, as /etc/pki is cert_t, which an unprivileged
# container reads only with container_read_certs, and ':Z' then relabels the copy, never the
# builder's own trust store. That bundle holds the proxy's CA when the bake has a proxy (up.sh).
install -D -m 0644 /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem "${certs}/ca.pem"

podman pull "${imager}"

set -- --rm -i --network=host \
    -e HTTP_PROXY -e HTTPS_PROXY -e NO_PROXY \
    -e SOURCE_DATE_EPOCH=1559424892 -e DETERMINISTIC_SEED=1 \
    -e SSL_CERT_FILE=/etc/imager-trust/ca.pem -e SSL_CERT_DIR=/etc/imager-trust \
    --volume "${certs}:/etc/imager-trust:ro,Z" \
    --volume "${out}:/out:Z"
if [ -e "${here}/talos-privileged" ]; then
    # Imagers before v1.13 attach a loop device. This VM is thrown away after the bake.
    set -- "$@" --privileged --volume /dev:/dev
else
    # The imager's image names no user, so it would run as root. Sidero's own run needs none.
    chown 65534:65534 "${out}"
    set -- "$@" --user 65534:65534
fi
podman run "$@" "${imager}" - < "${here}/talos-profile.yaml"

count=$(find "${out}" -maxdepth 1 -name '*.raw' | wc -l)
if [ "${count}" -ne 1 ]; then
    echo "the imager wrote ${count} raw images, not one" >&2
    exit 1
fi
raw=$(find "${out}" -maxdepth 1 -name '*.raw')

# The raw is the profile's disk size plus the imager's own partitions, so it is never the template's
# disk size. The rest of the disk stays as the volume left it, for Talos's data partition.
rawsize=$(wc -c < "${raw}")
disksize=$(blockdev --getsize64 "${target}")
if [ "${rawsize}" -gt "${disksize}" ]; then
    echo "the image of ${rawsize} bytes does not fit the disk of ${disksize} bytes" >&2
    exit 1
fi

# notrunc keeps the length of a target that is a file. It changes nothing on a block device.
dd if="${raw}" of="${target}" bs=4M conv=fsync,notrunc
cmp -n "${rawsize}" "${raw}" "${target}"
touch built
