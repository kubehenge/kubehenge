#!/bin/sh
# Fetches Sigstore's trusted root once into a file, and its signing config where a signer needs it,
# so that no cosign or gh of the run dials Sigstore's TUF server again. One dropped connection to it
# fails a client outright, which is why the fetch is retried here and only here. Only a failed
# connection is retried: an answer TUF refuses (an expired root, a bad signature on its metadata)
# would be refused the same way every time. A client that refuses a signature is never retried.
#
# It lives beside the node scripts because kubeadm nodes run it too, and the repository's checks
# and workflows run it from here.
#
#   sigstore-root.sh <trusted root file> [<signing config file>]
set -eu

output=${1:?usage: $0 <trusted root file> [<signing config file>]}
config=${2:-}
attempts=5
# Seconds one fetch may take. A server that accepts and never answers would otherwise hold it.
limit=60
# The errors of Go's HTTP client and of go-tuf's fetcher that a later attempt can cure. A connection
# closed before any answer is Go's bare EOF at the end of the line.
network='dial tcp|i/o timeout|connection refused|connection reset|no such host|network is unreachable|TLS handshake timeout|Client\.Timeout|unexpected EOF|: EOF$|proxyconnect|http status code: (429|5[0-9][0-9])'

command -v cosign > /dev/null || {
    echo "sigstore-root: cosign is not on PATH: scripts/tools.sh <dir> cosign installs it" >&2
    exit 1
}

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

attempt=1
while :; do
    rm -rf "${work}/tuf"
    mkdir "${work}/tuf"
    status=0
    TUF_ROOT=${work}/tuf timeout "${limit}" cosign initialize > "${work}/log" 2>&1 || status=$?
    if [ "${status}" -eq 0 ]; then
        # cosign keeps the files under a directory named for the mirror, so they are found by name.
        # gh reads a trusted root as JSON lines, and cosign reads that one line as well.
        found=$(find "${work}/tuf" -name trusted_root.json)
        tr -d '\r\n' < "${found}" > "${output}"
        echo >> "${output}"
        [ -z "${config}" ] || {
            found=$(find "${work}/tuf" -name 'signing_config*.json')
            cp "${found}" "${config}"
        }
        exit 0
    fi
    cat "${work}/log" >&2
    if [ "${status}" -eq 124 ]; then
        echo "sigstore-root: Sigstore gave no answer within ${limit} s" >&2
    elif ! grep -q -E "${network}" "${work}/log"; then
        echo "sigstore-root: cosign refused Sigstore's trusted root" >&2
        exit 1
    fi
    if [ "${attempt}" -ge "${attempts}" ]; then
        echo "sigstore-root: Sigstore is unreachable: no trusted root after ${attempts} attempts" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep 5
done
