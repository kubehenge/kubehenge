#!/bin/sh
# Puts a release of RKE2 or k3s where its bootstrap provider's air-gapped install takes it from, every
# file checked first against the release's checksum file, and that file and the install script
# against the SHA-256s Kubehenge's controller recorded when a cluster first used the release:
#
#   kubehenge-release [--mirror <address> [--mirror-ca <PEM file>]] rke2|k3s <version> \
#       <checksum file's SHA-256> <install script's SHA-256> <file>...
#
# A mirror serves the files on GitHub's paths beneath its address:
# <address>/<repository>/releases/download/<version>/<file> and <address>/<repository>/raw/<version>/install.sh.
set -eu

mirror=
mirror_ca=
while [ $# -gt 0 ]; do
    case "$1" in
        --mirror)
            mirror=$2
            shift 2
            ;;
        --mirror-ca)
            mirror_ca=$2
            shift 2
            ;;
        *) break ;;
    esac
done
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
if [ -n "${mirror}" ]; then
    release="${mirror}/${repository}/releases/download/${version}"
    install_script="${mirror}/${repository}/raw/${version}/install.sh"
    source="the release mirror ${mirror}"
else
    release="https://github.com/${repository}/releases/download/${version}"
    install_script="https://raw.githubusercontent.com/${repository}/${version}/install.sh"
    source=GitHub
fi
images="/var/lib/rancher/${distribution}/agent/images"
# On disk: /tmp may be kept in memory, and the images take hundreds of MB.
work=$(mktemp -d -p /var/tmp)
trap 'rm -rf "${work}"' EXIT

# fetch <url> <file>: downloads into ${work}. GitHub's release download address can answer 504 for
# hours while its API serves the same file, so once the address fails, this and every later release
# asset of the run comes through the API, by its tag and name. Curl takes the node's proxy
# settings from the environment for both. The API's limit without a token is 60 requests an hour
# per address, which is why the address is tried first.
release_down=
fetch() {
    if [ -n "${mirror}" ]; then
        mirror_fetch "$@"
        return
    fi
    case "$1" in
        https://github.com/*/*/releases/download/*/*)
            if [ -z "${release_down}" ]; then
                curl --fail --silent --show-error --location --proto '=https' --retry 2 --retry-all-errors --output "${work}/$2" "$1" && return 0
                release_down=1
            fi
            api_fetch "$1" "$2"
            ;;
        *) curl --fail --silent --show-error --location --proto '=https' --retry 8 --retry-all-errors --output "${work}/$2" "$1" ;;
    esac
}

# mirror_fetch <url> <file>: a mirror's answer of "not there" ends the download at once, while one
# that fails on the way is tried again. It may serve plain HTTP, since the pins check every file.
mirror_fetch() {
    set -- "$1" "$2" --fail --silent --show-error --location --proto '=http,https' --retry 8 --retry-connrefused \
        --write-out '%{http_code}' --output "${work}/$2"
    if [ -n "${mirror_ca}" ]; then
        set -- "$@" --cacert "${mirror_ca}"
    fi
    url=$1 name=$2
    shift 2
    code=$(curl "$@" "${url}") && return 0
    case "${code}" in
        404 | 410) echo "kubehenge-release: ${source} has no ${name} of ${repository} ${version} (${url})" >&2 ;;
        *) echo "kubehenge-release: ${source} did not serve ${name} of ${repository} ${version} (${url}, HTTP ${code:-none})" >&2 ;;
    esac
    return 1
}

# api_fetch <release download url> <file>
api_fetch() {
    path=${1#https://github.com/}
    api="https://api.github.com/repos/${path%%/releases/download/*}/releases"
    tag_asset=${path#*/releases/download/}
    curl --fail --silent --show-error --location --proto '=https' --retry 8 --retry-all-errors \
        --output "${work}/release.json" "${api}/tags/${tag_asset%%/*}"
    # The pretty-printed answer lists each asset's "url" before its "name".
    id=$(asset="${tag_asset#*/}" awk '
        /releases\/assets\/[0-9]+"/ { id = $0; sub(/.*releases\/assets\//, "", id); sub(/".*/, "", id) }
        $1 == "\"name\":" && $2 == "\"" ENVIRON["asset"] "\"," { print id; exit }' "${work}/release.json")
    [ -n "${id}" ] || {
        echo "kubehenge-release: ${tag_asset%%/*} has no asset ${tag_asset#*/}" >&2
        return 1
    }
    curl --fail --silent --show-error --location --proto '=https' --retry 8 --retry-all-errors \
        --header 'Accept: application/octet-stream' --output "${work}/$2" "${api}/assets/${id}"
}

fetch "${release}/sha256sum-amd64.txt" sha256sum-amd64.txt
fetch "${install_script}" install.sh
printf '%s  %s\n%s  %s\n' "${sums}" sha256sum-amd64.txt "${script}" install.sh > "${work}/recorded"
for file in "$@"; do
    awk -v file="${file}" '$2 == file { print; found = 1 } END { exit !found }' "${work}/sha256sum-amd64.txt" >> "${work}/listed" || {
        echo "kubehenge-release: ${version} lists no ${file}" >&2
        exit 1
    }
    fetch "${release}/${file}" "${file}"
done
if ! (cd "${work}" && sha256sum --check --strict --quiet recorded && sha256sum --check --strict --quiet listed); then
    echo "kubehenge-release: the files of ${repository} ${version} from ${source} do not match the pinned checksums. Nothing was installed" >&2
    exit 1
fi

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
