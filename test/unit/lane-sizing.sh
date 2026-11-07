#!/bin/sh
# Unit test of worker_memory_for in test/e2e/common.sh, which sizes a lane's workers. The worker
# column of the table in docs/testing.md must be what it computes from the chart's ClusterClasses
# for each add-on part, and GitOps must add what the test's own pods request. Needs helm, jq and yq.
#
#   test/unit/lane-sizing.sh
set -eu

here=$(dirname "$0")
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT
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

helm template kubehenge "${here}/../../charts/kubehenge" --namespace kubehenge-system \
    --set proxmox.url=https://pve.example:8006 --set templateRefresh.enabled=false \
    --set image=example.test/kubehenge@sha256:0000000000000000000000000000000000000000000000000000000000000000 > "${root}/chart.yaml"
for distribution in kubeadm rke2 k3s talos; do
    yq -o=json -I=0 "select(.kind == \"ClusterClass\" and .metadata.name == \"kubehenge-${distribution}\")" "${root}/chart.yaml" > "${root}/${distribution}.json"
done

fixtures=$((gitea_request_mib + gitops_agnhost_pods * agnhost_request_mib))
# lane <test> <distribution> <add-ons...>: the add-ons are those of the test's part in workload-cluster.sh.
lane() {
    lane_test=$1 lane_distribution=$2
    shift 2
    lane_gitops=false
    case " $* " in *" argocd "*) lane_gitops=true ;; *) ;; esac
    lane_on=$(jq -cn '$ARGS.positional + ["loadBalancer", "rancherAgent"]' --args "$@")
    lane_class=$(cat "${root}/${lane_distribution}.json")
    lane_wanted=$(sed -n "s/^| \`${lane_test}\` | \([0-9]*\) |.*/\1/p" "${here}/../../docs/testing.md")
    lane_got=$(worker_memory_for "${lane_class}" "${lane_on}" "${lane_gitops}")
    expect "${lane_test} against docs/testing.md" "${lane_wanted}" "${lane_got}"
    if [ "${lane_gitops}" = true ]; then
        # A GitOps worker never gets less than without it, nor more than the fixtures and one 8 MiB step.
        lane_without=$(worker_memory_for "${lane_class}" "${lane_on}" false)
        lane_grown=$((lane_got - lane_without))
        lane_fits=0
        if [ "${lane_grown}" -ge 0 ] && [ "${lane_grown}" -le $((fixtures + 7)) ]; then
            lane_fits=1
        fi
        expect "${lane_test} grows by the fixtures at most" 1 "${lane_fits}"
    fi
}
lane kubeadm-addons-a kubeadm headlamp monitoring logs certManager
lane kubeadm-addons-b kubeadm kyverno backups
lane kubeadm-addons-c kubeadm istio argocd
lane rke2-addons-a rke2 headlamp monitoring logs certManager
lane rke2-addons-b rke2 kyverno
lane rke2-addons-c rke2 istio backups
lane rke2-addons-d rke2 argocd
lane k3s-addons-a k3s headlamp monitoring logs certManager
lane k3s-addons-b k3s kyverno istio backups argocd
lane talos-addons-a talos headlamp monitoring logs certManager
lane talos-addons-b talos kyverno backups
lane talos-addons-c talos istio argocd

# A class whose requests set the size: the fixtures add to them, rounded up to 8 MiB.
class='{"metadata":{"annotations":{"kubehenge.dev/addon-memory":"{\"a\":100}","kubehenge.dev/node-memory":"{\"worker\":100}",
"kubehenge.dev/addon-memory-requests":"{\"a\":1000}","kubehenge.dev/node-memory-requests":"{\"worker\":500}"}},
"spec":{"variables":[{"name":"workerMachine","schema":{"openAPIV3Schema":{"properties":{"memoryMiB":{"minimum":1024}}}}}]}}'
synthetic=$(worker_memory_for "${class}" '["a"]' false)
expect "requests set the size" 1504 "${synthetic}"
synthetic=$(worker_memory_for "${class}" '["a"]' true)
expect "the fixtures add to the requests" 1712 "${synthetic}"

# The pods the sizing counts are the pods the script creates: a new fixture pod has to be listed in
# common.sh as running during the GitOps steps or not, or this fails.
created=$(sed -n 's/^[[:space:]]*agnhost_\(pod\|deployment\) \([a-z0-9-]*\) \([a-z0-9-]*\) .*/\2\/\3/p' "${here}/../e2e/workload-cluster.sh" | sort -u)
# shellcheck disable=SC2086 # one pod per word.
listed=$(printf '%s\n' ${gitops_fixture_pods} ${earlier_fixture_pods} | sort -u)
expect "every agnhost pod is listed in common.sh" "${created}" "${listed}"

printf 'lane-sizing: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
