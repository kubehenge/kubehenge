#!/bin/sh
# Downloads one file for the build, the CI and the tests:
#
#   scripts/fetch.sh <url> <output file>
#
# GitHub's release download address can answer 504 for hours while its API serves the same file.
# Once that address fails, this and every later GitHub release asset of the run comes through the
# API, without the address's retries, by its tag and name. GITHUB_TOKEN is sent when set, since the
# API's limit without one is 60 requests an hour per address. The caller checks the checksum.
#
# The switch has to outlive this process, because one make or script run starts many. It is a marker
# file: KUBEHENGE_FETCH_STATE names it, which scopes it to one run (tools.sh sets it). Without that
# variable the marker is a file in the temporary directory that counts for ten minutes, which spans
# the recipes of one make run and expires before a later run on a recovered address.
set -eu

url=${1:?usage: $0 <url> <output file>}
output=${2:?usage: $0 <url> <output file>}
marker=${KUBEHENGE_FETCH_STATE:-${TMPDIR:-/tmp}/kubehenge-fetch-releases-down}

# api <path> [curl option...]: a GET of GitHub's API.
api() {
    api_path=$1
    shift
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        set -- -H "Authorization: Bearer ${GITHUB_TOKEN}" "$@"
    fi
    curl -fsSL --proto '=https' --retry 8 --retry-all-errors --tlsv1.2 "$@" "https://api.github.com/${api_path}"
}

# release_asset: the asset of ${url} through the API, by its tag and name.
release_asset() {
    path=${url#https://github.com/}
    repository=${path%%/releases/download/*}
    tag_asset=${path#*/releases/download/}
    asset_id=$(api "repos/${repository}/releases/tags/${tag_asset%%/*}" |
        jq -r --arg name "${tag_asset#*/}" '.assets[] | select(.name == $name) | .id')
    if [ -z "${asset_id}" ]; then
        echo "fetch: ${tag_asset#*/} is not an asset of ${repository} ${tag_asset%%/*}" >&2
        return 1
    fi
    api "repos/${repository}/releases/assets/${asset_id}" -H 'Accept: application/octet-stream' -o "${output}"
}

# marked: whether the release address failed in this run.
marked() {
    if [ -n "${KUBEHENGE_FETCH_STATE:-}" ]; then
        [ -e "${marker}" ]
    else
        recent=$(find "${marker}" -mmin -10 2> /dev/null || true)
        [ -n "${recent}" ]
    fi
}

ok=0
case "${url}" in
    https://github.com/*/*/releases/download/*/*)
        # shellcheck disable=SC2310 # a failure of marked or curl is the branch taken.
        if ! marked; then
            if curl -fsSL --proto '=https' --retry 2 --retry-all-errors --tlsv1.2 -o "${output}" "${url}"; then
                ok=1
            else
                : > "${marker}"
            fi
        fi
        # shellcheck disable=SC2310 # its failure is the message below.
        if [ "${ok}" -eq 0 ] && release_asset; then ok=1; fi
        ;;
    *)
        if curl -fsSL --proto '=https' --retry 8 --retry-all-errors --tlsv1.2 -o "${output}" "${url}"; then ok=1; fi
        ;;
esac
if [ "${ok}" -eq 0 ]; then
    rm -f "${output}"
    echo "fetch: could not download ${url}" >&2
    exit 1
fi
