#!/bin/sh
# Unit test of scripts/pve-version.sh with a stand-in for curl serving a Packages index: the newest
# pve-manager version is found among many, a release newer than the cached image gives a key prefix
# that entry lacks, the same release gives one it has, and an index that cannot be read or lacks
# pve-manager is an error with no output.
#
#   test/unit/pve-version.sh
set -eu

script=$(cd "$(dirname "$0")/../../scripts" && pwd)/pve-version.sh
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# expect <what> <wanted> <got>
expect() {
    if [ "$3" = "$2" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}

# The stand-in copies the file STUB_INDEX to the -o file, or fails like an unreachable host while
# STUB_INDEX is empty.
mkdir "${root}/bin"
cat > "${root}/bin/curl" << 'EOF'
#!/bin/sh
out=''
while [ $# -gt 1 ]; do
    case "$1" in
        -o)
            out=$2
            shift
            ;;
        *) ;;
    esac
    shift
done
[ -n "${STUB_INDEX}" ] || exit 6
case "$1" in
    https://download.proxmox.com/debian/pve/dists/trixie/pve-no-subscription/binary-amd64/Packages.gz) cp "${STUB_INDEX}" "${out}" ;;
    *) exit 22 ;;
esac
EOF
chmod +x "${root}/bin/curl"
PATH=${root}/bin:${PATH}
export PATH

# index <file> <version...>: a gzipped index with pve-manager at each version, between the stanzas
# of other packages, one of which has a higher version.
index() {
    file=$1
    shift
    {
        printf 'Package: pve-container\nVersion: 99.0.0\n\n'
        for version in "$@"; do
            printf 'Package: pve-manager\nArchitecture: all\nVersion: %s\n\n' "${version}"
        done
        printf 'Package: pve-manager-extra\nVersion: 99.1.0\n\n'
    } | gzip -c > "${file}"
}
# expect_of <what> <wanted> <command...>: expect for what the command prints.
expect_of() {
    what=$1 wanted=$2
    shift 2
    got=$("$@")
    expect "${what}" "${wanted}" "${got}"
}
# run <index file> [hash]: the script's output, or "failed" when it fails.
run() {
    STUB_INDEX=$1 "${script}" "${2:-}" 2> "${root}/err" || echo failed
}
# prefix_of <index file>: the key prefix for the test's hash.
prefix_of() {
    run "$1" "${hash}" | sed -n 's/^prefix=//p'
}
# restored <index file>: whether the cached key starts with the key prefix of the index.
restored() {
    prefix=$(prefix_of "$1")
    case "${cached}" in
        "${prefix}"*) echo restored ;;
        *) echo rebuilt ;;
    esac
}
# last_error: the last line the script wrote to the standard error.
last_error() {
    tail -n 1 "${root}/err"
}

hash=0123abcd

index "${root}/older" 9.0.0~10 9.2.4 9.2.41 9.2.9
index "${root}/newer" 9.0.0~10 9.2.4 9.2.41 9.3.1 9.2.9
index "${root}/pre" 9.0.0~9 9.0.0~10

# The cached image was saved under the key of the release it was built from.
cached="e2e-pve-${hash}-9.2.41-1234567"

expect_of "the newest of many, by Debian order" "version=9.2.41" run "${root}/older"
expect_of "a pre-release sorts before its release" "version=9.0.0~10" run "${root}/pre"
expect_of "the hash adds the key prefix" "version=9.3.1
prefix=e2e-pve-${hash}-9.3.1-" run "${root}/newer" "${hash}"
expect_of "a newer release: no cache entry matches" rebuilt restored "${root}/newer"
expect_of "the same release: the cache entry matches" restored restored "${root}/older"

indexurl=https://download.proxmox.com/debian/pve/dists/trixie/pve-no-subscription/binary-amd64/Packages.gz
expect_of "an unreadable index fails" failed run ''
expect_of "an unreadable index says which" "pve-version: could not read ${indexurl}" last_error

printf 'Package: pve-container\nVersion: 1\n' | gzip -c > "${root}/none"
expect_of "an index without pve-manager fails" failed run "${root}/none" "${hash}"
expect_of "an index without pve-manager says so" "pve-version: ${indexurl} lists no pve-manager" last_error

printf 'not gzip' > "${root}/bad"
expect_of "an index that is no gzip file fails" failed run "${root}/bad" "${hash}"
expect_of "an index that is no gzip file says so" "pve-version: ${indexurl} is not a gzip file" last_error

printf 'pve-version: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
