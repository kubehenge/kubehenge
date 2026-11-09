#!/bin/sh
# Unit test of scripts/providers.sh with a stand-in for curl: the mirror gets each package the lock
# file pins for the platform, at OpenTofu's packed layout, and a CLI configuration with no direct
# installation. A package already there is not fetched again. A package or registry checksum the
# lock file does not pin is refused, and the mirror drops files it no longer pins.
#
#   test/unit/providers.sh
set -eu

providers=$(cd "$(dirname "$0")/../../scripts" && pwd)/providers.sh
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
# expect_of <what> <wanted> <command...>: expect for what the command prints.
expect_of() {
    what=$1 wanted=$2
    shift 2
    got=$("$@")
    expect "${what}" "${wanted}" "${got}"
}
sum() {
    printf '%s' "$1" | sha256sum | cut -d ' ' -f 1
}

# The registry offers each package of a/one and b/two for linux_amd64 and windows_amd64, with the
# package's bytes being its own name. STUB_BYTES replaces what a download returns, STUB_SUM the
# checksum the registry names.
mkdir "${root}/bin"
cat > "${root}/bin/curl" << 'EOF'
#!/bin/sh
out=''
while [ $# -gt 1 ]; do
    if [ "$1" = -o ]; then
        out=$2
        shift
    fi
    shift
done
url=$1
printf '%s\n' "${url}" >> "${STUB_LOG}"
case "${url}" in
    https://registry.opentofu.org/v1/providers/*/download/*)
        rest=${url#https://registry.opentofu.org/v1/providers/}
        type=${rest#*/}
        type=${type%%/*}
        version=${rest#*/*/}
        version=${version%%/*}
        platform=$(printf '%s' "${rest#*/download/}" | tr / _)
        file=terraform-provider-${type}_${version}_${platform}.zip
        shasum=${STUB_SUM:-$(printf '%s' "${file}" | sha256sum | cut -d ' ' -f 1)}
        printf '{"filename":"%s","download_url":"https://example.org/%s","shasum":"%s"}' "${file}" "${file}" "${shasum}" > "${out}"
        ;;
    https://example.org/*) printf '%s' "${STUB_BYTES:-${url#https://example.org/}}" > "${out}" ;;
    *) exit 22 ;;
esac
EOF
chmod +x "${root}/bin/curl"
PATH=${root}/bin:${PATH}
export PATH

one_linux=terraform-provider-one_1.0.0_linux_amd64.zip
one_windows=terraform-provider-one_1.0.0_windows_amd64.zip
two_linux=terraform-provider-two_2.0.0_linux_amd64.zip
one_linux_sum=$(sum "${one_linux}")
one_windows_sum=$(sum "${one_windows}")
two_linux_sum=$(sum "${two_linux}")
other_sum=$(sum other)
# lock <file> <provider block...>: a lock file in OpenTofu's format.
lock() {
    out=$1
    shift
    : > "${out}"
    for block in "$@"; do
        # shellcheck disable=SC2086 # a block is its words.
        set -- ${block}
        address=$1 version=$2
        shift 2
        {
            printf 'provider "%s" {\n  version     = "%s"\n  constraints = "~> %s"\n  hashes = [\n' "${address}" "${version}" "${version}"
            printf '    "h1:unused=",\n'
            for h in "$@"; do printf '    "zh:%s",\n' "${h}"; done
            printf '  ]\n}\n\n'
        } >> "${out}"
    done
}
lock "${root}/lock.hcl" \
    "registry.opentofu.org/a/one 1.0.0 ${one_linux_sum} ${one_windows_sum}" \
    "registry.opentofu.org/b/two 2.0.0 ${two_linux_sum}"

log="${root}/log"
mirror="${root}/mirror"
# The path OpenTofu reads, native on Windows.
native=${mirror}
if command -v cygpath > /dev/null 2>&1; then native=$(cygpath -m "${mirror}"); fi
# run [<variable=value>...]: providers.sh for linux_amd64 against the stand-in.
run() {
    : > "${log}"
    env STUB_LOG="${log}" "$@" "${providers}" "${root}/lock.hcl" "${mirror}" linux_amd64 > /dev/null 2> "${root}/err"
}
# status <command...>: ok or failed.
status() {
    if "$@"; then echo ok; else echo failed; fi
}
calls() {
    grep -c -F -- "$1" "${log}" || true
}
files() {
    (cd "${mirror}" && find . -type f | sort | tr '\n' ' ')
}

# shellcheck disable=SC2310 # its exit is what is checked.
expect_of "a fresh mirror: succeeds" ok status run
expect_of "a fresh mirror: holds the platform's packages and the configuration" \
    "./registry.opentofu.org/a/one/${one_linux} ./registry.opentofu.org/b/two/${two_linux} ./tofu.tfrc " files
expect_of "a fresh mirror: the package's bytes" "${one_linux}" cat "${mirror}/registry.opentofu.org/a/one/${one_linux}"
expect_of "the configuration names the mirror" 1 grep -c -F "path    = \"${native}\"" "${mirror}/tofu.tfrc"
expect_of "the configuration has no direct installation" 0 sh -c "grep -c 'direct *{' '${mirror}/tofu.tfrc' || true"
expect_of "the registry is asked for the platform" 1 calls https://registry.opentofu.org/v1/providers/a/one/1.0.0/download/linux/amd64

# shellcheck disable=SC2310 # its exit is what is checked.
expect_of "a second run: succeeds" ok status run
expect_of "a second run: fetches nothing" "" cat "${log}"

rm -f "${mirror}/registry.opentofu.org/b/two/${two_linux}"
# shellcheck disable=SC2310 # its exit is what is checked.
expect_of "a package that does not match: fails" failed status run STUB_BYTES=tampered
expect_of "a package that does not match: is not kept" absent sh -c "[ -e '${mirror}/registry.opentofu.org/b/two/${two_linux}' ] && echo present || echo absent"
expect_of "a package that does not match: names it" \
    "providers: https://example.org/${two_linux} does not match the lock file's checksum ${two_linux_sum}" cat "${root}/err"

# shellcheck disable=SC2310 # its exit is what is checked.
expect_of "a registry checksum the lock file lacks: fails" failed status run STUB_SUM="${other_sum}"
expect_of "a registry checksum the lock file lacks: nothing downloaded" 0 calls https://example.org/

printf 'stale' > "${mirror}/registry.opentofu.org/b/two/terraform-provider-two_1.9.0_linux_amd64.zip"
mkdir -p "${mirror}/registry.opentofu.org/c/gone"
printf 'gone' > "${mirror}/registry.opentofu.org/c/gone/terraform-provider-gone_1.0.0_linux_amd64.zip"
printf '%s' "${one_windows}" > "${mirror}/registry.opentofu.org/a/one/${one_windows}"
# shellcheck disable=SC2310 # its exit is what is checked.
expect_of "a mirror with other files: succeeds" ok status run
expect_of "a mirror with other files: keeps what the lock pins, for any platform" \
    "./registry.opentofu.org/a/one/${one_linux} ./registry.opentofu.org/a/one/${one_windows} ./registry.opentofu.org/b/two/${two_linux} ./tofu.tfrc " files
expect_of "a mirror with other files: drops the provider's empty directory" absent \
    sh -c "[ -e '${mirror}/registry.opentofu.org/c' ] && echo present || echo absent"

lock "${root}/lock.hcl" "registry.terraform.io/a/one 1.0.0 ${one_linux_sum}"
# shellcheck disable=SC2310 # its exit is what is checked.
expect_of "a provider of another registry: fails" failed status run
expect_of "a provider of another registry: names it" \
    "providers: ${root}/lock.hcl pins registry.terraform.io/a/one 1.0.0 without a version and zh: hashes from registry.opentofu.org" cat "${root}/err"

printf 'providers: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
