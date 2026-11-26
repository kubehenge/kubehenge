#!/bin/sh
# Fetches Sigstore's trusted root once into a file, for cosign verify --trusted-root, so that no
# verify of the run dials Sigstore's TUF server again. One dropped connection to it fails a
# `cosign verify` outright, which is why the fetch is retried here and only here. A verify that
# refuses a signature is never retried.
#
#   scripts/sigstore-root.sh <output file>
#
# SIGSTORE_ROOT_ATTEMPTS (default 5) and SIGSTORE_ROOT_WAIT in seconds (default 5) tune the retries.
set -eu

output=${1:?usage: $0 <output file>}
attempts=${SIGSTORE_ROOT_ATTEMPTS:-5}
wait=${SIGSTORE_ROOT_WAIT:-5}

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
    # cosign keeps the file under a directory named for the mirror, so it is found by name.
    if TUF_ROOT=${work}/tuf cosign initialize > "${work}/log" 2>&1 &&
        found=$(find "${work}/tuf" -name trusted_root.json | head -n 1) && [ -n "${found}" ]; then
        cp "${found}" "${output}"
        exit 0
    fi
    if [ "${attempt}" -ge "${attempts}" ]; then
        cat "${work}/log" >&2
        echo "sigstore-root: Sigstore is unreachable: no trusted root after ${attempts} attempts" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep "${wait}"
done
