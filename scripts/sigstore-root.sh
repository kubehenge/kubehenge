#!/bin/sh
# Fetches Sigstore's trusted root once into a file, for cosign verify --trusted-root, so that no
# verify of the run dials Sigstore's TUF server again. One dropped connection to it fails a
# `cosign verify` outright, which is why the fetch is retried here and only here. Only a failed
# connection is retried: an answer TUF refuses (an expired root, a bad signature on its metadata)
# would be refused the same way every time. A verify that refuses a signature is never retried.
#
#   scripts/sigstore-root.sh <output file>
set -eu

output=${1:?usage: $0 <output file>}
attempts=5
# The errors of Go's HTTP client and of go-tuf's fetcher that a later attempt can cure.
network='dial tcp|i/o timeout|connection refused|connection reset|no such host|network is unreachable|TLS handshake timeout|Client\.Timeout|unexpected EOF|proxyconnect|http status code: (429|5[0-9][0-9])'

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
    if TUF_ROOT=${work}/tuf cosign initialize > "${work}/log" 2>&1; then
        # cosign keeps the file under a directory named for the mirror, so it is found by name.
        found=$(find "${work}/tuf" -name trusted_root.json | head -n 1)
        [ -n "${found}" ] || {
            cat "${work}/log" >&2
            echo "sigstore-root: cosign initialize wrote no trusted_root.json" >&2
            exit 1
        }
        cp "${found}" "${output}"
        exit 0
    fi
    cat "${work}/log" >&2
    grep -q -E "${network}" "${work}/log" || {
        echo "sigstore-root: cosign refused Sigstore's trusted root" >&2
        exit 1
    }
    if [ "${attempt}" -ge "${attempts}" ]; then
        echo "sigstore-root: Sigstore is unreachable: no trusted root after ${attempts} attempts" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep 5
done
