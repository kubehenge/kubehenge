#!/bin/sh
# Unit test of flux_ready and flux_state in test/e2e/common.sh, which wait for the Flux Operator's
# FluxInstance and say what holds it up when a test fails, against a stand-in kubectl.
#
#   test/unit/flux-state.sh
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

# A kubectl that answers for the FluxInstance with ${root}/instance.json, or as one that finds none
# while that file is missing, and for flux-system only while ${root}/namespace is there.
cat > "${root}/bin/kubectl" << EOF
#!/bin/sh
case "\$*" in
    'get namespace flux-system') [ -e "${root}/namespace" ] ;;
    '-n flux-system get fluxinstance flux -o json')
        if [ ! -e "${root}/instance.json" ]; then
            echo 'Error from server (NotFound): fluxinstances.fluxcd.controlplane.io "flux" not found' >&2
            exit 1
        fi
        cat "${root}/instance.json"
        ;;
    '-n flux-system get gitrepositories,kustomizations')
        echo 'NAME                                     URL   AGE   READY   STATUS'
        echo 'gitrepository/flux-system   ssh://git@gitea:2222/e2e/clusters.git   9m   False   ssh: handshake failed'
        ;;
    '-n flux-system get pods -o json') cat "${root}/pods.json" ;;
    '-n flux-system get events --sort-by=.metadata.creationTimestamp')
        echo '5m   Warning   FailedScheduling   pod/source-controller-7c9   0/2 nodes are available: 1 Insufficient memory.'
        ;;
    '-n flux-system logs deployment/flux-operator --tail=40') echo 'health check failed after 5m0s' ;;
    *) exit 1 ;;
esac
EOF
chmod +x "${root}/bin/kubectl"
PATH="${root}/bin:${PATH}"

# ready <kubeconfig>: flux_ready's answer, then what it left in seen.
ready() {
    seen=
    # shellcheck disable=SC2310 # the answer is what is tested.
    if flux_ready "$1"; then answer=ready; else answer=waits; fi
    printf '%s; %s' "${answer}" "${seen}" | tr -d '\r'
}

got=$(ready "${root}/kubeconfig")
expect "a FluxInstance not yet made waits, with the API server's answer" \
    'waits; Error from server (NotFound): fluxinstances.fluxcd.controlplane.io "flux" not found' "${got}"

echo '{"status": {}}' > "${root}/instance.json"
got=$(ready "${root}/kubeconfig")
expect "a FluxInstance the operator has not reconciled waits" "waits; Unknown -: no Ready condition yet" "${got}"

cat > "${root}/instance.json" << 'EOF'
{"status": {"conditions": [
  {"type": "Reconciling", "status": "True", "reason": "Progressing", "message": "Reconciliation in progress"},
  {"type": "Ready", "status": "False", "reason": "ReconciliationFailed",
   "message": "reconciliation failed: timeout waiting for: [GitRepository/flux-system/flux-system status: 'Failed']\nssh: handshake failed"}]}}
EOF
got=$(ready "${root}/kubeconfig")
expect "a FluxInstance not ready waits, with its Ready condition on one line" \
    "waits; False ReconciliationFailed: reconciliation failed: timeout waiting for: [GitRepository/flux-system/flux-system status: 'Failed'] ssh: handshake failed" "${got}"

long=$(printf '%0500d' 0)
printf '{"status": {"conditions": [{"type": "Ready", "status": "False", "reason": "Progressing", "message": "%s"}]}}' "${long}" > "${root}/instance.json"
got=$(ready "${root}/kubeconfig")
cut_seen=${got#waits; }
expect "a long message is cut to 400 characters" 400 "${#cut_seen}"

echo '{"status": {"conditions": [{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "message": "Reconciliation finished"}]}}' > "${root}/instance.json"
got=$(ready "${root}/kubeconfig")
expect "a ready FluxInstance is ready" "ready; " "${got}"

got=$(flux_state "${root}/kubeconfig")
expect "a cluster without flux-system prints nothing" "" "${got}"

touch "${root}/namespace"
cat > "${root}/instance.json" << 'EOF'
{"status": {"conditions": [{"type": "Ready", "status": "False", "reason": "ReconciliationFailed", "message": "health check failed"}]}}
EOF
cat > "${root}/pods.json" << 'EOF'
{"items": [
  {"metadata": {"name": "flux-operator-5d8"}, "spec": {"nodeName": "worker-1"}, "status": {"phase": "Running",
   "conditions": [{"type": "Ready", "status": "True"}],
   "containerStatuses": [{"name": "manager", "ready": true, "restartCount": 2, "state": {"running": {"startedAt": "2026-11-07T00:30:00Z"}},
     "lastState": {"terminated": {"reason": "Error", "exitCode": 137, "finishedAt": "2026-11-07T00:29:58Z"}}}]}},
  {"metadata": {"name": "helm-controller-6f4"}, "spec": {"nodeName": "worker-1"}, "status": {"phase": "Pending",
   "conditions": [{"type": "Ready", "status": "False", "reason": "ContainersNotReady", "message": "containers with unready status: [manager]"}],
   "containerStatuses": [{"name": "manager", "ready": false, "restartCount": 0,
     "state": {"waiting": {"reason": "ImagePullBackOff", "message": "Back-off pulling image ghcr.io/fluxcd/helm-controller"}}, "lastState": {}}]}},
  {"metadata": {"name": "source-controller-7c9"}, "spec": {}, "status": {"phase": "Pending",
   "conditions": [{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/2 nodes are available: 1 Insufficient memory."}]}}
]}
EOF
got=$(flux_state "${root}/kubeconfig" | tr -d '\r')
expect "a failed test shows the FluxInstance, its sync, each pod's state, the events and the operator" "
Flux:
FluxInstance Ready=False ReconciliationFailed: health check failed
NAME                                     URL   AGE   READY   STATUS
gitrepository/flux-system   ssh://git@gitea:2222/e2e/clusters.git   9m   False   ssh: handshake failed
flux-operator-5d8 on worker-1: Running
  manager: running, ready true, 2 restarts, last ended Error with exit 137 at 2026-11-07T00:29:58Z
helm-controller-6f4 on worker-1: Pending
  Ready ContainersNotReady: containers with unready status: [manager]
  manager: waiting ImagePullBackOff (Back-off pulling image ghcr.io/fluxcd/helm-controller), ready false, 0 restarts
source-controller-7c9 on no node: Pending
  PodScheduled Unschedulable: 0/2 nodes are available: 1 Insufficient memory.
5m   Warning   FailedScheduling   pod/source-controller-7c9   0/2 nodes are available: 1 Insufficient memory.
health check failed after 5m0s" "${got}"

rm "${root}/instance.json"
got=$(flux_state "${root}/kubeconfig" | tr -d '\r' | sed -n 3p)
expect "a FluxInstance that cannot be read shows why" \
    'FluxInstance: Error from server (NotFound): fluxinstances.fluxcd.controlplane.io "flux" not found' "${got}"

printf 'flux-state: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
