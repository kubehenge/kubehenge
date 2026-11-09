#!/bin/sh
# Checks that Sidero signed every image a Talos pin names. Each pin line of the pins file holds the
# digests of Sidero's imager, installer base and extensions image and the QEMU guest agent's, and
# cosign verifies each digest against the signer management-cluster/talos.tf trusts for
# ghcr.io/siderolabs/*. cosign reads a digest and ignores any tag beside it, so every reference
# here is a digest alone. Sidero's tags move, which is why a signature is required. Needs cosign
# (scripts/tools.sh installs it) and the network, for Sigstore's trust root and transparency log.
#
#   scripts/verify-talos-pins.sh [<pins file> [<talos.tf>]]
set -eu

fail() {
    echo "verify-talos-pins: $*" >&2
    exit 1
}

command -v cosign > /dev/null || fail "cosign is not on PATH: scripts/tools.sh <dir> cosign installs it"

root=$(cd "$(dirname "$0")/.." && pwd)
pins=${1:-${root}/pkg/provisioner/releasepins.txt}
talos_tf=${2:-${root}/management-cluster/talos.tf}
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

signer='image = "ghcr.io/siderolabs/*"'
count=$(grep -c -F "${signer}" "${talos_tf}") || count=0
[ "${count}" -eq 1 ] || fail "${talos_tf} names ${count} signers of ghcr.io/siderolabs/*, want one"
line=$(grep -F "${signer}" "${talos_tf}")
issuer=$(printf '%s\n' "${line}" | sed -n 's|.*issuer = "\([^"]*\)".*|\1|p')
# HCL writes a backslash twice, and cosign wants it once.
regex=$(printf '%s\n' "${line}" | sed -n 's|.*subjectRegex = "\([^"]*\)".*|\1|p' | sed 's|\\\\|\\|g')
[ -n "${issuer}" ] && [ -n "${regex}" ] || fail "${talos_tf} gives no issuer and subject regex for ghcr.io/siderolabs/*"

# The pins as the Go parser reads them: no blank or comment lines, and nothing from " #" on. A line
# of three fields is an RKE2 or k3s pin, and one of five a Talos pin.
sed 's|^[[:space:]]*||; s| #.*$||' "${pins}" | awk 'NF && !/^#/' > "${work}/lines"
bad=$(awk 'NF != 3 && NF != 5' "${work}/lines")
[ -z "${bad}" ] || fail "${pins} has a pin line of neither three fields nor five: ${bad}"
awk 'NF == 5' "${work}/lines" > "${work}/talos"
[ -s "${work}/talos" ] || fail "${pins} holds no Talos pin"

while read -r release imager installer extensions agent; do
    printf '%s imager ghcr.io/siderolabs/imager@%s\n' "${release}" "${imager}"
    printf '%s installer-base ghcr.io/siderolabs/installer-base@%s\n' "${release}" "${installer}"
    printf '%s extensions ghcr.io/siderolabs/extensions@%s\n' "${release}" "${extensions}"
    printf '%s guest-agent ghcr.io/siderolabs/qemu-guest-agent@%s\n' "${release}" "${agent#*@}"
done < "${work}/talos" > "${work}/jobs"

tuf=$(mktemp -d "${work}/tuf.XXXXXX")
TUF_ROOT=${tuf} cosign initialize > "${work}/trust-root.log" 2>&1 || {
    cat "${work}/trust-root.log" >&2
    fail "cosign could not fetch Sigstore's trust root"
}

# Each call gets a copy of the trust root of its own, so that no two share a cache. A refusal exits
# 1, which cosign also does for an unreachable Sigstore, so the log says which it was. The text is
# for the shell xargs starts, which expands it.
# shellcheck disable=SC2016
verify_one='
release=$1 name=$2 ref=$3
log=${work}/${release}-${name}.log
TUF_ROOT=$(mktemp -d "${work}/tuf.XXXXXX")
export TUF_ROOT
cp -R "${tuf}/." "${TUF_ROOT}/"
cosign verify "${ref}" --certificate-oidc-issuer "${issuer}" --certificate-identity-regexp "${regex}" > "${log}" 2>&1 || {
    cat "${log}" >&2
    echo "verify-talos-pins: cosign refused ${name} of ${release}" >&2
    exit 1
}
'
export work tuf issuer regex
xargs -n 3 -P 8 sh -c "${verify_one}" verify < "${work}/jobs" || fail "cosign refused an image of ${pins}, or Sigstore could not be reached"

awk '{ print $1 ": imager, installer base, extensions and agent signed" }' "${work}/talos"
