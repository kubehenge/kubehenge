#!/bin/sh
# Unit test of scripts/verify-providers.sh and the trust-root fetch it shares, run on copies of both
# beside a stub fetch.sh, with stub gh, sha256sum, cosign and sleep that record their calls. Needs no
# network.
#
#   test/unit/verify-providers.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "${here}/../.." && pwd)
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

# A copy of the repository's layout, so that the script's fetch.sh is the stub.
mkdir -p "${tmp}/repo/scripts" "${tmp}/repo/pkg/templates/node" "${tmp}/repo/management-cluster" "${tmp}/bin"
cp "${root}/scripts/verify-providers.sh" "${tmp}/repo/scripts/"
cp "${root}/pkg/templates/node/sigstore-root.sh" "${tmp}/repo/pkg/templates/node/"
cp "${root}/management-cluster/versions.tf" "${tmp}/repo/management-cluster/"
script=${tmp}/repo/scripts/verify-providers.sh

# The provider images versions.tf pins, which every downloaded components file names.
grep -o 'ghcr.io/kubehenge/[a-z0-9-]*:[^"]*@sha256:[0-9a-f]*' "${tmp}/repo/management-cluster/versions.tf" |
    sed 's|^|    image: |' > "${tmp}/images"
cat > "${tmp}/repo/scripts/fetch.sh" << 'EOF'
#!/bin/sh
echo "fetch $1" >> "${CALLS}"
cp "${IMAGES}" "$2"
EOF
cat > "${tmp}/bin/gh" << 'EOF'
#!/bin/sh
echo "gh $*" >> "${CALLS}"
EOF
cat > "${tmp}/bin/sha256sum" << 'EOF'
#!/bin/sh
cat > /dev/null
EOF
# cosign refuses a verify whose arguments hold ${STUB_REFUSE} and fails the first ${STUB_INIT_FAIL}
# fetches of the trust root with ${STUB_INIT_ERROR}, a dropped connection by default.
cat > "${tmp}/bin/cosign" << 'EOF'
#!/bin/sh
case $1 in
    initialize)
        echo "trust-root" >> "${CALLS}"
        done=$(grep -c '^trust-root$' "${CALLS}")
        if [ "${done}" -le "${STUB_INIT_FAIL:-0}" ]; then
            echo "${STUB_INIT_ERROR:-tuf refresh failed: dial tcp 34.117.62.14:443: i/o timeout}" >&2
            exit 1
        fi
        mkdir -p "${TUF_ROOT}/mirror/targets"
        echo '{}' > "${TUF_ROOT}/mirror/targets/trusted_root.json"
        ;;
    verify)
        shift
        echo "verify $*" >> "${CALLS}"
        case "$*" in
            *"${STUB_REFUSE:-@none@}"*)
                echo "no matching signatures" >&2
                exit 1
                ;;
        esac
        ;;
esac
EOF
cat > "${tmp}/bin/sleep" << 'EOF'
#!/bin/sh
echo "sleep $*" >> "${CALLS}"
EOF
chmod +x "${tmp}/bin/"* "${tmp}/repo/scripts/"*.sh "${tmp}/repo/pkg/templates/node/"*.sh

# run: the script's output in out, its status in status and the stubs' calls in calls.
run() {
    : > "${tmp}/calls"
    status=0
    out=$(PATH="${tmp}/bin:${PATH}" CALLS="${tmp}/calls" IMAGES="${tmp}/images" sh "${script}" 2>&1) || status=$?
    calls=$(cat "${tmp}/calls")
}

cd "${tmp}"

run
same "a good run exits 0" 0 "${status}"
same "one trust-root fetch" 1 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
same "one verify for each fork" 2 "$(grep -c -e '^verify ' "${tmp}/calls" || true)"
roots=$(printf '%s\n' "${calls}" | grep '^verify ' | sed -n 's/.* --trusted-root \([^ ]*\) .*/\1/p' | sort -u)
same "every verify reads the one trusted root file" 1 "$(printf '%s\n' "${roots}" | grep -c . || true)"
same "each verify names a trusted root" 2 "$(grep -c -e ' --trusted-root ' "${tmp}/calls" || true)"
same "six gh checks of provenance, files and image of each fork" 6 "$(grep -c -e '^gh attestation[ ]verify ' "${tmp}/calls" || true)"
same "each gh check names a trusted root" 6 "$(grep -c -e '^gh attestation verify .* --custom-trusted-root ' "${tmp}/calls" || true)"
roots=$(printf '%s\n' "${calls}" | sed -n -e 's/.* --trusted-root \([^ ]*\) .*/\1/p' -e 's/.* --custom-trusted-root \([^ ]*\).*/\1/p' | sort -u)
same "gh and cosign read the one trusted root file" 1 "$(printf '%s\n' "${roots}" | grep -c . || true)"
mentions "the signer of the fork's release workflow" "--certificate-identity https://github.com/kubehenge/cluster-api-bootstrap-provider-talos/.github/workflows/release.yaml@refs/tags/v" "${calls}"
mentions "GitHub's issuer" "--certificate-oidc-issuer https://token.actions.githubusercontent.com" "${calls}"
mentions "a fork's line" "files, image and signatures verified" "${out}"

STUB_INIT_FAIL=1 run
same "a dropped first fetch of the trust root passes" 0 "${status}"
same "the fetch was retried once" 2 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
same "both verifies still ran" 2 "$(grep -c -e '^verify ' "${tmp}/calls" || true)"

STUB_INIT_FAIL=99 run
same "an unreachable Sigstore exits 1" 1 "${status}"
mentions "it names Sigstore as unreachable" "Sigstore is unreachable" "${out}"
mentions "it says the fetch failed" "verify-providers: could not fetch Sigstore's trust root" "${out}"
same "the fetch is capped at five attempts" 5 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
lacks "no signature is called missing" "carries no" "${out}"
same "nothing is checked after a failed fetch" 0 "$(grep -c -e '^\(verify\|gh\|fetch\) ' "${tmp}/calls" || true)"

STUB_INIT_FAIL=99 STUB_INIT_ERROR="tuf: failed to decode root.json: expired at 2025-08-19 14:33:09 +0000 UTC" run
same "a trust root TUF refuses exits 1" 1 "${status}"
same "a trust root TUF refuses is fetched once" 1 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
mentions "it prints cosign's message" "expired at 2025-08-19" "${out}"
lacks "it does not call Sigstore unreachable" "unreachable" "${out}"

STUB_REFUSE=cluster-api-bootstrap-provider-talos@sha256 run
same "a refusal exits 1" 1 "${status}"
mentions "a refusal names the image" "carries no cosign signature of kubehenge/cluster-api-bootstrap-provider-talos" "${out}"
mentions "a refusal prints cosign's log" "no matching signatures" "${out}"
same "a refused signature is not retried" 1 "$(grep -c -e '^verify .*cluster-api-bootstrap-provider-talos@sha256' "${tmp}/calls" || true)"
same "a refusal fetches the trust root once" 1 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"

printf '%d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
