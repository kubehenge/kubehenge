#!/bin/sh
# Unit test of webhook_retried in test/e2e/common.sh, which tries a write to the API again while
# an admission webhook cannot be reached, against a stand-in kubectl.
#
#   test/unit/webhook-retried.sh
set -eu

here=$(dirname "$0")
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT
mkdir "${root}/bin"
# shellcheck source=../e2e/common.sh
. "${here}/../e2e/common.sh"

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

# A kubectl whose webhook does not answer for the first ${root}/down tries, then patches with a
# warning, or refuses when asked to.
cat > "${root}/bin/kubectl" << EOF
#!/bin/sh
tries=\$(cat "${root}/tries" 2> /dev/null || echo 0)
echo \$((tries + 1)) > "${root}/tries"
if [ "\${tries}" -lt "\$(cat "${root}/down")" ]; then
    echo 'Error from server (InternalError): Internal error occurred: failed calling webhook "default.cluster.cluster.x-k8s.io": failed to call webhook: Post "https://capi-webhook-service.capi-system.svc:443/mutate": dial tcp 10.108.71.203:443: connect: connection refused' >&2
    exit 1
fi
case "\$*" in
    *refused*)
        echo 'Error from server (Forbidden): admission webhook "validation.kubehenge.dev" denied the request: refused' >&2
        exit 1
        ;;
esac
echo 'Warning: Replaces the only control plane' >&2
echo 'cluster.cluster.x-k8s.io/e2e patched'
EOF
chmod +x "${root}/bin/kubectl"
PATH="${root}/bin:${PATH}"

# write <tries the webhook is down> <kubectl arguments...>: runs the write through webhook_retried,
# leaving its status, the tries made, its output and its errors in status, tries, out and errors.
write() {
    echo "$1" > "${root}/down"
    shift
    rm -f "${root}/tries"
    status=0
    # shellcheck disable=SC2310 # its status is what is checked, and webhook_retried needs no set -e.
    webhook_retried kubectl "$@" > "${root}/out" 2> "${root}/errors" || status=$?
    tries=$(cat "${root}/tries")
    out=$(cat "${root}/out")
    errors=$(cat "${root}/errors")
}

write 2 patch cluster/e2e
expect "a write is tried again until the webhook answers" "0 3 cluster.cluster.x-k8s.io/e2e patched" "${status} ${tries} ${out}"
waits=$(grep -c 'did not answer, trying again' "${root}/errors")
last=$(tail -n 1 "${root}/errors")
expect "each wait is said, and the write's own warning kept" "2 Warning: Replaces the only control plane" "${waits} ${last}"

write 0 patch cluster/e2e refused
expect "a refusal returns at once, with its error and status" \
    "1 1 Error from server (Forbidden): admission webhook \"validation.kubehenge.dev\" denied the request: refused" \
    "${status} ${tries} ${errors}"

KUBEHENGE_E2E_WEBHOOK_WAIT=0
write 100 patch cluster/e2e
refused=$(grep -c 'connect: connection refused' "${root}/errors")
expect "past the wait, the webhook's error fails the write" "1 1 1" "${status} ${tries} ${refused}"

printf 'webhook-retried: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
