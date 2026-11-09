#!/bin/sh
# Lays out the OpenTofu providers a lock file pins as a filesystem mirror, with a CLI configuration
# that has OpenTofu install providers from it alone:
#
#   scripts/providers.sh <lock file> <mirror directory> [<platform>]
#
# OpenTofu's own install takes each package and its checksums from GitHub's release address, without
# a token. It fails for as long as that address answers 504. Here the registry names the package and
# scripts/fetch.sh downloads it, through GitHub's API once the address fails. The package must match
# a zh: hash of the lock file, which OpenTofu checks again as it installs it.
#
# The platform is tofu's own (TOFU selects the binary) unless given, as linux_amd64. The mirror keeps
# the packages of other platforms the lock file still pins, and drops every other file.
# <mirror directory>/tofu.tfrc is the configuration for TF_CLI_CONFIG_FILE.
set -eu

usage="usage: $0 <lock file> <mirror directory> [<platform>]"
lock=${1:?${usage}}
mirror=${2:?${usage}}
# Without the configuration this writes, which tofu warns of on its standard output while missing.
platform=${3:-$(env -u TF_CLI_CONFIG_FILE "${TOFU:-tofu}" version -json | jq -r .platform)}
here=$(cd "$(dirname "$0")" && pwd)
case "${platform}" in
    *_*) ;;
    *)
        echo "providers: ${platform} is no platform such as linux_amd64" >&2
        exit 1
        ;;
esac
mkdir -p "${mirror}"
mirror=$(cd "${mirror}" && pwd)
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

# Each provider as one line: its address, its version and its zh: hashes.
awk '/^provider "/ { gsub(/"/, "", $2); address = $2; version = ""; hashes = "" }
    /^ *version *=/ && address != "" { gsub(/"/, "", $3); version = $3 }
    /"zh:/ && address != "" { gsub(/[",]|zh:/, "", $1); hashes = hashes " " $1 }
    /^}/ && address != "" { print address, version hashes; address = "" }' "${lock}" > "${work}/providers"
[ -s "${work}/providers" ] || {
    echo "providers: ${lock} pins no providers" >&2
    exit 1
}

# sha256 <file>
sha256() {
    sha256sum "$1" | cut -d ' ' -f 1
}

while read -r address version hashes; do
    host=${address%%/*} name=${address#*/}
    if [ "${host}" != registry.opentofu.org ] || [ -z "${version}" ] || [ -z "${hashes}" ]; then
        echo "providers: ${lock} pins ${address} ${version} without a version and zh: hashes from registry.opentofu.org" >&2
        exit 1
    fi
    type=${name#*/}
    file=terraform-provider-${type}_${version}_${platform}.zip
    target=${mirror}/${address}/${file}
    if [ -f "${target}" ]; then
        have=$(sha256 "${target}")
        case " ${hashes} " in
            *" ${have} "*) continue ;;
            *) rm -f "${target}" ;;
        esac
    fi
    "${here}/fetch.sh" "https://${host}/v1/providers/${name}/${version}/download/${platform%%_*}/${platform#*_}" "${work}/download.json"
    url=$(jq -r --arg file "${file}" 'select(.filename == $file) | .download_url' "${work}/download.json")
    sum=$(jq -r --arg file "${file}" 'select(.filename == $file) | .shasum' "${work}/download.json")
    if [ -z "${url}" ]; then
        echo "providers: the registry offers no ${file} for ${address} ${version}" >&2
        exit 1
    fi
    case " ${hashes} " in
        *" ${sum} "*) ;;
        *)
            echo "providers: the registry's checksum of ${file} is none the lock file pins for ${address}" >&2
            exit 1
            ;;
    esac
    "${here}/fetch.sh" "${url}" "${work}/package.zip"
    have=$(sha256 "${work}/package.zip")
    if [ "${have}" != "${sum}" ]; then
        echo "providers: ${url} does not match the lock file's checksum ${sum}" >&2
        exit 1
    fi
    mkdir -p "${mirror}/${address}"
    mv "${work}/package.zip" "${target}"
    echo "providers: ${address} ${version} for ${platform}"
done < "${work}/providers"

# A package stays only while the lock file pins it, for this platform or another.
cut -d ' ' -f 3- "${work}/providers" | tr ' ' '\n' > "${work}/pinned"
find "${mirror}" -type f ! -path "${mirror}/tofu.tfrc" > "${work}/files"
while read -r present; do
    have=$(sha256 "${present}")
    grep -qxF "${have}" "${work}/pinned" || rm -f "${present}"
done < "${work}/files"
find "${mirror}" -mindepth 1 -type d -empty -delete

# OpenTofu on Windows reads the path natively.
path=${mirror}
if command -v cygpath > /dev/null 2>&1; then path=$(cygpath -m "${mirror}"); fi
cat > "${work}/tofu.tfrc" << EOF
# Written by scripts/providers.sh. Without a direct block, no provider comes from the network.
provider_installation {
  filesystem_mirror {
    path    = "${path}"
    include = ["registry.opentofu.org/*/*"]
  }
}
EOF
mv "${work}/tofu.tfrc" "${mirror}/tofu.tfrc"
