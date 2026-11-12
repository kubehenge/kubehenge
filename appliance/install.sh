#!/bin/sh
# Installs Kubehenge's appliance on a Proxmox VE node: a small unprivileged Debian container running
# Kubehenge's web interface, which then sets up the management cluster from the browser.
#
# Run as root in the node's shell:
#
#   sh install.sh [options]
#
# This script downloads the release it carries, checks it against its checksum and hands over to
# `kubehenge install`, which asks its questions on a terminal and installs. `sh install.sh --help`
# lists the options. --release <file> --sha256 <hex> installs a local release archive instead of
# the published one.
set -eu

# The release this script installs, and the OpenTofu the installer runs the management-cluster
# module with.
KUBEHENGE_VERSION=0.1.0
KUBEHENGE_SHA256=
# renovate: datasource=github-release-attachments depName=opentofu/opentofu
TOFU_TAG=v1.13.2
TOFU_SHA256=ac05d9952662c7db28dd57e93f0a68018837bfdf9342384ec0987977c16a9645
# The unpacked release needs 1 GiB beside the downloaded archive.
work_kib=1048576

fail() {
    echo "kubehenge: $*" >&2
    exit 1
}

# proxy_label <proxy URL>: scheme://host:port of it, without credentials or path, as safe to print.
proxy_label() {
    case "$1" in
        *://*)
            label_scheme=${1%%://*}
            label_rest=${1#*://}
            ;;
        *)
            label_scheme=http
            label_rest=$1
            ;;
    esac
    label_rest=${label_rest##*@}
    printf '%s://%s' "${label_scheme}" "${label_rest%%/*}"
}

# read_download_proxy <datacenter options file>: sets $download_proxy to the HTTP proxy of Proxmox
# VE's datacenter options, empty when the environment names one (curl reads that itself) or none is
# set. It is passed to curl alone: exported, it would send the requests to the container and to the
# Proxmox VE API through the proxy too.
read_download_proxy() {
    download_proxy=''
    [ -z "${https_proxy:-}${HTTPS_PROXY:-}${http_proxy:-}" ] || return 0
    download_proxy=$(sed -n 's/^http_proxy:[[:space:]]*\(.*[^[:space:]]\)[[:space:]]*$/\1/p' "$1" 2> /dev/null | head -n 1)
    [ -n "${download_proxy}" ] || return 0
    shown=$(proxy_label "${download_proxy}")
    echo "kubehenge: downloading through ${shown}, the HTTP proxy of the datacenter options"
}

# fetch <url> <file> <SHA-256> <what it is>: downloads and checks a file, with a progress bar on a
# terminal. Any failed transfer is tried again, a connection reset mid-way included, which --retry
# alone leaves out. HTTPS only.
# GitHub's release download address can answer 504 for hours while its API serves the same file, so
# once that address fails, the file comes through the API, by its tag and name, over the same proxy.
# The API's limit without a token is 60 requests an hour per address, which is why the address is
# tried first.
fetch() {
    progress=-s
    [ ! -t 2 ] || progress=--progress-bar
    echo "downloading $4"
    retries=8
    github=
    case "$1" in
        https://github.com/*/*/releases/download/*/*)
            github=1
            retries=2
            ;;
        *) ;;
    esac
    rc=0 http=''
    # shellcheck disable=SC2086 # the unquoted expansion is two words on purpose, the URL one of them.
    http=$(curl -fSL ${download_proxy:+--proxy "${download_proxy}"} "${progress}" --proto '=https' --retry "${retries}" --retry-all-errors --tlsv1.2 -w '%{http_code}' -o "$2" "$1") || rc=$?
    if [ "${rc}" -ne 0 ] && [ -n "${github}" ]; then
        # shellcheck disable=SC2310 # its failure is the message below.
        if release_asset "$1" "$2" "${progress}"; then rc=0; fi
    fi
    if [ "${rc}" -ne 0 ]; then
        # 000 is curl's word for no HTTP answer at all.
        [ "${http}" != 000 ] || http=''
        also=''
        [ -z "${github}" ] || also=", nor through GitHub's API"
        fail "could not download $1 (curl exit ${rc}${http:+, HTTP status ${http}})${also}"
    fi
    echo "$3  $2" | sha256sum -c --quiet - || fail "$1 does not match its checksum"
    echo "$4: checksum verified"
}

# release_asset <release download url> <file> <progress option>: a GitHub release asset through the
# API. GITHUB_TOKEN is sent when set.
release_asset() {
    asset_path=${1#https://github.com/}
    asset_tag_name=${asset_path#*/releases/download/}
    asset_api="https://api.github.com/repos/${asset_path%%/releases/download/*}/releases"
    # shellcheck disable=SC2086 # the unquoted expansions are two words on purpose, the URL one of them.
    curl -fsSL ${download_proxy:+--proxy "${download_proxy}"} ${GITHUB_TOKEN:+--header "Authorization: Bearer ${GITHUB_TOKEN}"} --proto '=https' --retry 8 --retry-all-errors --tlsv1.2 \
        -o "$2.release" "${asset_api}/tags/${asset_tag_name%%/*}" || return 1
    # The pretty-printed answer lists each asset's "url" before its "name".
    asset_id=$(asset="${asset_tag_name#*/}" awk '
        /releases\/assets\/[0-9]+"/ { id = $0; sub(/.*releases\/assets\//, "", id); sub(/".*/, "", id) }
        $1 == "\"name\":" && $2 == "\"" ENVIRON["asset"] "\"," { print id; exit }' "$2.release")
    rm -f "$2.release"
    [ -n "${asset_id}" ] || return 1
    # shellcheck disable=SC2086 # as above.
    curl -fSL ${download_proxy:+--proxy "${download_proxy}"} ${GITHUB_TOKEN:+--header "Authorization: Bearer ${GITHUB_TOKEN}"} "$3" --proto '=https' --retry 8 --retry-all-errors --tlsv1.2 \
        --header 'Accept: application/octet-stream' -o "$2" "${asset_api}/assets/${asset_id}"
}

release='' release_sha=''
for arg in "$@"; do
    if [ -n "${expect:-}" ]; then
        case "${expect}" in
            release) release=${arg} ;;
            sha256) release_sha=${arg} ;;
            *) ;;
        esac
        expect=''
        continue
    fi
    case "${arg}" in
        --release) expect=release ;;
        --sha256) expect=sha256 ;;
        *) ;;
    esac
done

uid=$(id -u)
[ "${uid}" -eq 0 ] || fail "run this as root on a Proxmox VE node"
command -v pct > /dev/null || fail "this is not a Proxmox VE node"
if [ -n "${release}" ]; then
    [ -n "${release_sha}" ] || fail "--release needs --sha256"
    KUBEHENGE_SHA256=${release_sha}
fi
[ -n "${KUBEHENGE_SHA256}" ] || fail "this script carries no release checksum; use --release <file> --sha256 <hex>"

work=$(mktemp -d)
free_kib=$(df -Pk "${work}" 2> /dev/null | awk 'NR == 2 { print $4 }')
if [ -n "${free_kib}" ] && [ "${free_kib}" -lt "${work_kib}" ]; then
    rm -rf "${work}"
    fail "${TMPDIR:-/tmp} has $((free_kib / 1024)) MiB free and the release needs $((work_kib / 1024)) MiB. Free space there, or set TMPDIR to a directory on a larger file system."
fi

read_download_proxy /etc/pve/datacenter.cfg
if [ -n "${release}" ]; then
    cp "${release}" "${work}/kubehenge.tar.gz" || fail "cannot read ${release}"
    echo "${KUBEHENGE_SHA256}  ${work}/kubehenge.tar.gz" | sha256sum -c --quiet - || fail "${release} does not match its checksum"
    echo "${release}: checksum verified"
else
    fetch "https://github.com/kubehenge/kubehenge/releases/download/v${KUBEHENGE_VERSION}/kubehenge_${KUBEHENGE_VERSION}_linux_amd64.tar.gz" \
        "${work}/kubehenge.tar.gz" "${KUBEHENGE_SHA256}" "Kubehenge ${KUBEHENGE_VERSION}"
fi
mkdir "${work}/kubehenge"
tar -xzf "${work}/kubehenge.tar.gz" -C "${work}/kubehenge"
rm -f "${work}/kubehenge.tar.gz"

# The installer removes the directory when it ends.
KUBEHENGE_BOOTSTRAP_DIR=${work} KUBEHENGE_RELEASE_DIR=${work}/kubehenge KUBEHENGE_TOFU_TAG=${TOFU_TAG} KUBEHENGE_TOFU_SHA256=${TOFU_SHA256}
export KUBEHENGE_BOOTSTRAP_DIR KUBEHENGE_RELEASE_DIR KUBEHENGE_TOFU_TAG KUBEHENGE_TOFU_SHA256
exec "${work}/kubehenge/bin/kubehenge" install "$@"
