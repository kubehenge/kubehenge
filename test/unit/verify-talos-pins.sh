#!/bin/sh
# Unit test of scripts/verify-talos-pins.sh, with a stub cosign that records its calls: which pins
# the script reads, which references it verifies and with what, and how it fails. Needs no network.
#
#   test/unit/verify-talos-pins.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "${here}/../.." && pwd)
script=${root}/scripts/verify-talos-pins.sh
real_pins=${root}/pkg/provisioner/releasepins.txt
real_tf=${root}/management-cluster/talos.tf
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

passes=0 failures=0
# same <what> <wanted> <got>
same() {
    if [ "$2" = "$3" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}
# mentions <what> <needle> <text>
mentions() {
    case $3 in
        *"$2"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
            failures=$((failures + 1))
            ;;
    esac
}
# lacks <what> <needle> <text>
lacks() {
    case $3 in
        *"$2"*)
            printf 'FAIL: %s\n  unwanted: %s\n  in:       %s\n' "$1" "$2" "$3"
            failures=$((failures + 1))
            ;;
        *) passes=$((passes + 1)) ;;
    esac
}

# The stub appends a line to ${CALLS} for each call. It refuses a verify whose arguments hold
# ${STUB_REFUSE}, and fails initialize when STUB_INIT_FAIL is set.
mkdir "${tmp}/bin"
cat > "${tmp}/bin/cosign" << 'EOF'
#!/bin/sh
case $1 in
    initialize)
        echo "initialize" >> "${CALLS}"
        if [ -n "${STUB_INIT_FAIL:-}" ]; then
            echo "no route to the TUF root" >&2
            exit 1
        fi
        ;;
    verify)
        shift
        echo "verify $* TUF=${TUF_ROOT}" >> "${CALLS}"
        case "$*" in
            *"${STUB_REFUSE:-@none@}"*)
                echo "no matching signatures" >&2
                exit 1
                ;;
        esac
        ;;
esac
EOF
chmod +x "${tmp}/bin/cosign"

# SHA-256s in hex, each one character repeated.
d1=1111111111111111111111111111111111111111111111111111111111111111
d2=2222222222222222222222222222222222222222222222222222222222222222
d3=3333333333333333333333333333333333333333333333333333333333333333
d4=4444444444444444444444444444444444444444444444444444444444444444
d5=5555555555555555555555555555555555555555555555555555555555555555
d6=6666666666666666666666666666666666666666666666666666666666666666
d7=7777777777777777777777777777777777777777777777777777777777777777
d8=8888888888888888888888888888888888888888888888888888888888888888
da=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
db=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# The real header, whose comment lines hold five fields and four, then two such lines of our own.
header=$(sed -n '/^#/!q;p' "${real_pins}")
write_pins() {
    file=$1
    shift
    {
        printf '%s\n' "${header}"
        printf '# has to be pinned.\n# rke2 = v1.37.1+rke2r1\n\n'
        printf 'v1.14.5 sha256:%s sha256:%s sha256:%s 11.1.1@sha256:%s\n' "${d1}" "${d2}" "${d3}" "${d4}"
        printf 'v1.13.9 sha256:%s sha256:%s sha256:%s 10.2.0@sha256:%s # added by a test\n' "${d5}" "${d6}" "${d7}" "${d8}"
        printf 'v1.37.1+rke2r1 %s %s\n' "${da}" "${db}"
        [ $# -eq 0 ] || printf '%s\n' "$@"
    } > "${file}"
}
pins=${tmp}/pins.txt
write_pins "${pins}"

# run <pins> <talos.tf>: the script's output in out and its status in status.
run() {
    : > "${tmp}/calls"
    status=0
    out=$(PATH="${tmp}/bin:${PATH}" CALLS="${tmp}/calls" sh "${script}" "$@" 2>&1) || status=$?
    calls=$(cat "${tmp}/calls")
}

# The script runs from a directory that holds nothing of the repository, so that it finds its own root.
cd "${tmp}"

run "${pins}"
same "a good run exits 0" 0 "${status}"
same "one initialize" 1 "$(grep -c '^initialize$' "${tmp}/calls" || true)"
same "eight verifies, four for each Talos pin" 8 "$(grep -c '^verify ' "${tmp}/calls" || true)"
lacks "no header word is read as a pin" "imager@to" "${calls}"
lacks "the RKE2 pin is not verified" "${da}" "${calls}"
tufs=$(printf '%s\n' "${calls}" | sed -n 's/.* TUF=//p' | sort | uniq -d)
same "no two verifies share a cache" "" "${tufs}"
same "each verify has a cache" 8 "$(grep -c ' TUF=/' "${tmp}/calls" || true)"
flags=$(printf '%s\n' "${calls}" | grep '^verify ' | grep -o -e ' --[a-z-]*' | sort -u | tr -d '\n')
same "only the issuer and the identity regex" " --certificate-identity-regexp --certificate-oidc-issuer" "${flags}"
mentions "the issuer of talos.tf" "--certificate-oidc-issuer https://accounts.google.com " "${calls}"
mentions "the regex of talos.tf, its backslashes single" '--certificate-identity-regexp ^(releasemgr-svc@talos-production\.iam\.gserviceaccount\.com|[^@]+@siderolabs\.com)$ ' "${calls}"
mentions "the agent by its digest alone" "verify ghcr.io/siderolabs/qemu-guest-agent@sha256:${d4} " "${calls}"
lacks "no agent tag" "qemu-guest-agent:" "${calls}"
mentions "the imager by digest" "verify ghcr.io/siderolabs/imager@sha256:${d1} " "${calls}"
mentions "a release's line" "v1.13.9: imager, installer base, extensions and agent signed" "${out}"

STUB_REFUSE="installer-base@sha256:${d6}" run "${pins}"
same "a refusal exits 1, not xargs's 123" 1 "${status}"
mentions "a refusal names its image and release" "cosign refused installer-base of v1.13.9" "${out}"
mentions "a refusal prints cosign's log" "no matching signatures" "${out}"

STUB_INIT_FAIL=1 run "${pins}"
same "a failed initialize exits 1" 1 "${status}"
mentions "a failed initialize says so" "could not fetch Sigstore's trust root" "${out}"
lacks "no verify after a failed initialize" "verify " "${calls}"

four=${tmp}/four.txt
write_pins "${four}" "v1.12.1 sha256:${d1} sha256:${d2} sha256:${d3}"
run "${four}"
same "a pin of four fields exits 1" 1 "${status}"
mentions "it names the line" "v1.12.1 sha256:${d1}" "${out}"

none=${tmp}/none.tf
grep -v -F 'image = "ghcr.io/siderolabs/*"' "${real_tf}" > "${none}"
run "${pins}" "${none}"
same "no signer exits 1" 1 "${status}"
mentions "no signer names its count" "names 0 signers" "${out}"
same "no signer, no cosign call" "" "${calls}"
two=${tmp}/two.tf
{
    cat "${real_tf}"
    grep -F 'image = "ghcr.io/siderolabs/*"' "${real_tf}"
} > "${two}"
run "${pins}" "${two}"
same "two signers exit 1" 1 "${status}"
mentions "two signers name their count" "names 2 signers" "${out}"
same "two signers, no cosign call" "" "${calls}"

# A PATH with the script's other tools and no cosign.
mkdir "${tmp}/tools"
for tool in sh grep sed awk xargs cat mktemp rm dirname cp tr; do
    target=$(command -v "${tool}")
    ln -s "${target}" "${tmp}/tools/${tool}"
done
# Git for Windows makes a link a copy, which needs the runtime library beside it.
[ ! -e /usr/bin/msys-2.0.dll ] || cp /usr/bin/msys-2.0.dll "${tmp}/tools/"
status=0
out=$(PATH="${tmp}/tools" CALLS="${tmp}/calls" "${tmp}/tools/sh" "${script}" "${pins}" 2>&1) || status=$?
same "no cosign exits 1" 1 "${status}"
mentions "no cosign says so" "cosign is not on PATH" "${out}"
lacks "no cosign names no release" "v1.14.5" "${out}"

printf '%d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
