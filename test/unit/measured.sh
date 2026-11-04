#!/bin/sh
# Unit test of measured in test/e2e/common.sh, which takes a measurement without ever failing the
# test, and of the readings it runs: pod_memory and control_plane_use fail when a kubelet does not
# answer, against a stand-in kubectl.
#
#   test/unit/measured.sh
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

# A kubectl whose kubelets answer once ${root}/up is there, and otherwise as one whose serving
# certificate is not back.
cat > "${root}/bin/kubectl" << EOF
#!/bin/sh
case "\$*" in
    'get nodes -o jsonpath='*) echo cp-1 ;;
    'get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='*) echo cp-1 ;;
    'get --raw /api/v1/nodes/cp-1/proxy/stats/summary')
        if [ ! -e "${root}/up" ]; then
            echo 'Error from server (ServiceUnavailable): error trying to reach service: remote error: tls: internal error' >&2
            exit 1
        fi
        echo '{"node": {"memory": {"workingSetBytes": 2147483648, "availableBytes": 1073741824}}, "pods": [{"podRef": {"namespace": "kube-system", "name": "cilium-1"}, "memory": {"workingSetBytes": 209715200}}]}'
        ;;
    *) exit 1 ;;
esac
EOF
chmod +x "${root}/bin/kubectl"
PATH="${root}/bin:${PATH}"
export KUBEHENGE_E2E_MEASURE_EVERY=0

KUBEHENGE_E2E_MEASURE_TIMEOUT=1
got=$(measured "memory with the CNI alone" pod_memory "${root}/kubeconfig")
expect "a kubelet that never answers leaves the measurement unavailable, with its error" \
    "memory with the CNI alone: unavailable after 1 s: Error from server (ServiceUnavailable): error trying to reach service: remote error: tls: internal error" "${got}"
measured "memory with the CNI alone" pod_memory "${root}/kubeconfig" > /dev/null
expect "and the test goes on under set -e" "on" on
got=$(measured "" control_plane_use "${root}/kubeconfig" kyverno)
expect "a reading without a heading is named by its command" \
    "control_plane_use: unavailable after 1 s: Error from server (ServiceUnavailable): error trying to reach service: remote error: tls: internal error" "${got}"
got=$(measured "a reading with no output" false)
expect "a reading that fails silently says so" "a reading with no output: unavailable after 1 s: no output" "${got}"

KUBEHENGE_E2E_MEASURE_TIMEOUT=20
(sleep 2 && touch "${root}/up") &
# jq on Windows ends its lines with CR LF.
got=$(measured "memory with the CNI alone" pod_memory "${root}/kubeconfig" | tr -d '\r')
wait
expect "a kubelet that answers later gives the figures, under the heading" "memory with the CNI alone:
cp-1: 2048 MiB of memory in use, 1024 MiB available
  kube-system: 200 MiB in 1 pods" "${got}"
got=$(measured "" control_plane_use "${root}/kubeconfig" kyverno)
expect "and a reading without a heading only its figures" "with kyverno: cp-1 has 2048 MiB of memory in use" "${got}"

printf 'measured: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
