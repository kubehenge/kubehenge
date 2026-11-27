#!/bin/sh
# Unit test of the Sigstore steps of pkg/templates/node/images.sh and the trust-root fetch beside it
# that it runs. Stubs on PATH for curl (which hands over a stub cosign), sha256sum, kubeadm, crictl,
# mktemp, sed, systemctl and sleep record their calls. Needs no network and touches nothing outside
# its own directory.
#
#   test/unit/node-images.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
script=$(cd "${here}/../.." && pwd)/pkg/templates/node/images.sh
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

mkdir "${tmp}/bin" "${tmp}/stub"
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
mv "${tmp}/bin/cosign" "${tmp}/stub/cosign"
cat > "${tmp}/bin/curl" << 'EOF'
#!/bin/sh
while [ $# -gt 0 ]; do
    case $1 in
        --output) out=$2 ;;
        *) ;;
    esac
    shift
done
cp "${COSIGN_STUB}" "${out}"
EOF
cat > "${tmp}/bin/sha256sum" << 'EOF'
#!/bin/sh
cat > /dev/null
EOF
cat > "${tmp}/bin/kubeadm" << 'EOF'
#!/bin/sh
printf 'registry.k8s.io/kube-apiserver:v1.35.0\nregistry.k8s.io/pause:3.10\n'
EOF
cat > "${tmp}/bin/crictl" << 'EOF'
#!/bin/sh
echo "crictl $*" >> "${CALLS}"
case $1 in
    inspecti)
        for image; do :; done
        echo "${image%:*}@sha256:1111111111111111111111111111111111111111111111111111111111111111"
        ;;
    *) ;;
esac
EOF
# The script's own directory goes under ${STUB_TMP}, not /var/tmp.
cat > "${tmp}/bin/mktemp" << 'EOF'
#!/bin/sh
mkdir "${STUB_TMP}/work.$$"
echo "${STUB_TMP}/work.$$"
EOF
cat > "${tmp}/bin/sed" << 'EOF'
#!/bin/sh
echo "sed $*" >> "${CALLS}"
EOF
cat > "${tmp}/bin/systemctl" << 'EOF'
#!/bin/sh
echo "systemctl $*" >> "${CALLS}"
EOF
chmod +x "${tmp}/bin/"* "${tmp}/stub/cosign"

# run: the script's output in out, its status in status and the stubs' calls in calls.
run() {
    : > "${tmp}/calls"
    status=0
    out=$(PATH="${tmp}/bin:${PATH}" CALLS="${tmp}/calls" COSIGN_STUB="${tmp}/stub/cosign" STUB_TMP="${tmp}" \
        sh "${script}" v1.35.0 2>&1) || status=$?
    calls=$(cat "${tmp}/calls")
}

run
same "a good run exits 0" 0 "${status}"
same "one trust-root fetch" 1 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
same "a verify for each image" 2 "$(grep -c -e '^verify ' "${tmp}/calls" || true)"
same "each verify names a trusted root" 2 "$(grep -c -e ' --trusted-root ' "${tmp}/calls" || true)"
mentions "Kubernetes' signer" "--certificate-identity krel-trust@k8s-releng-prod.iam.gserviceaccount.com" "${calls}"
mentions "the pause image is written into containerd's configuration" "sed -i" "${calls}"

STUB_INIT_FAIL=1 run
same "a dropped first fetch of the trust root passes" 0 "${status}"
same "the fetch was retried once" 2 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
same "both verifies still ran" 2 "$(grep -c -e '^verify ' "${tmp}/calls" || true)"

STUB_INIT_FAIL=99 run
same "an unreachable Sigstore exits 1" 1 "${status}"
mentions "it names Sigstore as unreachable" "sigstore-root: Sigstore is unreachable" "${out}"
mentions "it says the fetch failed" "kubehenge-images: could not fetch Sigstore's trusted root" "${out}"
same "the fetch is capped at five attempts" 5 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
lacks "no signature is called missing" "has no signature" "${out}"
same "no image is pulled or deleted" 0 "$(grep -c -e '^crictl ' "${tmp}/calls" || true)"

STUB_INIT_FAIL=99 STUB_INIT_ERROR="tuf: failed to decode root.json: expired at 2025-08-19 14:33:09 +0000 UTC" run
same "a trust root TUF refuses exits 1" 1 "${status}"
same "a trust root TUF refuses is fetched once" 1 "$(grep -c -e '^trust-root$' "${tmp}/calls" || true)"
mentions "it prints cosign's message" "expired at 2025-08-19" "${out}"
mentions "it says cosign refused the root" "sigstore-root: cosign refused Sigstore's trusted root" "${out}"
lacks "it does not call Sigstore unreachable" "unreachable" "${out}"
same "no image is pulled" 0 "$(grep -c -e '^crictl ' "${tmp}/calls" || true)"

STUB_REFUSE=kube-apiserver@sha256 run
same "a refusal exits 1" 1 "${status}"
mentions "a refusal names the image" "kube-apiserver@sha256:1111111111111111111111111111111111111111111111111111111111111111 has no signature" "${out}"
same "a refused signature is not retried" 1 "$(grep -c -e '^verify .*kube-apiserver@sha256' "${tmp}/calls" || true)"
same "the refused image is deleted" 1 "$(grep -c -e '^crictl rmi ' "${tmp}/calls" || true)"

# The node runs the fetch from beside the installed script, as the test runs it from the repository.
kubeadm=$(cat "${here}/../../pkg/templates/node/kubeadm.sh")
# shellcheck disable=SC2016 # kubeadm.sh's own text, which the node's shell expands.
mentions "kubeadm installs the script" 'install -D -m 0755 "${files}/node/images.sh" /usr/libexec/kubehenge/images' "${kubeadm}"
# shellcheck disable=SC2016 # the same.
mentions "kubeadm installs the fetch beside it" 'install -D -m 0755 "${files}/node/sigstore-root.sh" /usr/libexec/kubehenge/sigstore-root.sh' "${kubeadm}"

printf '%d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
