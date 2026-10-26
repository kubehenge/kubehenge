#!/bin/sh
# Helpers every end-to-end test uses. Sourced after `set -eu`.

# The images the tests run their own pods from.
# renovate: datasource=docker depName=registry.k8s.io/e2e-test-images/agnhost
agnhost=registry.k8s.io/e2e-test-images/agnhost:2.66.1@sha256:6e2eef87d0dc77e070a7549bc5fdfa290c0fefe3359eb33c2ad1eda692fe66a4
# renovate: datasource=docker depName=registry.k8s.io/pause
pause_image=registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
# The Git server the GitOps engines sync from, rootless as Pod Security's restricted level needs.
# renovate: datasource=docker depName=docker.io/gitea/gitea
gitea=docker.io/gitea/gitea:28.2.0-rootless@sha256:94807314454f6525994a3b76286ba12bc7e7f8cd14f61d9e67431767f8103306
export agnhost pause_image gitea

# Each step shows the time since the test started, which is how a slow run shows where it lost it.
e2e_started=$(date +%s)
step() {
    elapsed=$(($(date +%s) - e2e_started))
    printf '\n== [%d:%02d] %s\n' $((elapsed / 60)) $((elapsed % 60)) "$*"
}
fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

# until_true <timeout seconds> <description> <command...>: retries the command until it succeeds,
# 2 seconds after the first try, then 4 and 8, then every 10: most waits end within seconds, and
# the long ones cost no more checks than a steady 10-second poll.
until_true() {
    limit=$(($(date +%s) + $1)) what=$2 pause=2
    shift 2
    until "$@"; do
        now=$(date +%s)
        [ "${now}" -lt "${limit}" ] || fail "timed out: ${what}"
        sleep "${pause}"
        pause=$((pause < 8 ? pause * 2 : 10))
    done
}

# new_namespace <kubectl function> <namespace>: creates the namespace and waits for its default
# service account. The service account controller adds it just after the namespace, and until then
# the API server refuses pods there, which a pod created straight away can lose to.
new_namespace() {
    "$1" create namespace "$2"
    until_true 60 "namespace $2's default service account" has_default_account "$1" "$2"
}
has_default_account() { "$1" -n "$2" get serviceaccount default > /dev/null 2>&1; }

# class_providers <ClusterClass>: the namespaces of the Cluster API providers only that class's
# clusters use, as Kubehenge's controller is told them.
class_providers() {
    kubectl -n kubehenge-system get deployment/kubehenge-controller -o json |
        jq -r --arg c "$1" '.spec.template.spec.containers[0].args[] | select(startswith("--class-providers=" + $c + "=")) | split("=")[2] | split(",")[]'
}

# providers_idle <ClusterClass>: whether those providers run no pod, as they should while no
# cluster uses the class.
providers_idle() {
    namespaces=$(class_providers "$1")
    [ -n "${namespaces}" ] || fail "the controller is told of no providers for $1"
    for ns in ${namespaces}; do
        kubectl -n "${ns}" get deployments -l cluster.x-k8s.io/provider -o json |
            jq -e '.items | length > 0 and all(.spec.replicas == 0 and (.status.replicas // 0) == 0)' > /dev/null || return 1
    done
}

# pod_memory <kubeconfig>: each node's memory in use, then the memory the pods of each namespace use
# over all nodes, from the kubelets' working sets: the figures add-ons are sized by. Kubehenge's
# metrics proxies in kube-system are counted apart, as monitoring's. The load balancer's pods are
# listed one by one, since its figure is its controller's and one node's share.
pod_memory() {
    pods='[]'
    for node in $(KUBECONFIG="$1" kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
        summary=$(KUBECONFIG="$1" kubectl get --raw "/api/v1/nodes/${node}/proxy/stats/summary")
        printf '%s' "${summary}" |
            jq -r --arg n "${node}" '.node.memory | "\($n): \(.workingSetBytes / 1048576 | floor) MiB of memory in use, \(.availableBytes / 1048576 | floor) MiB available"'
        pods=$(printf '%s' "${summary}" | jq -c --argjson all "${pods}" '$all + [.pods[] | {
            ns: (if .podRef.namespace == "kube-system" and (.podRef.name | startswith("kubehenge-")) then "kube-system (Kubehenge metrics proxies)" else .podRef.namespace end),
            name: .podRef.name, bytes: (.memory.workingSetBytes // 0)}]')
    done
    printf '%s' "${pods}" | jq -r 'group_by(.ns)[] | "  \(.[0].ns): \(map(.bytes) | add / 1048576 | floor) MiB in \(length) pods"'
    printf '%s' "${pods}" | jq -r '.[] | select(.ns == "metallb-system" or (.ns == "kube-system" and (.name | startswith("kube-vip")))) |
        "    \(.name): \(.bytes / 1048576 | floor) MiB"'
}

# control_plane_use <kubeconfig> <add-on>: each control plane's memory in use once the add-on is
# on, from its kubelet's working set. From one add-on to the next it grows by what the class's
# kubehenge.dev/addon-control-plane-memory figures size.
control_plane_use() {
    for node in $(KUBECONFIG="$1" kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[*].metadata.name}'); do
        KUBECONFIG="$1" kubectl get --raw "/api/v1/nodes/${node}/proxy/stats/summary" |
            jq -r --arg n "${node}" --arg a "$2" '"with \($a): \($n) has \(.node.memory.workingSetBytes / 1048576 | floor) MiB of memory in use"'
    done
}

# control_plane_record <kubeconfig>: until killed, every 15 s, each control plane's memory in use
# with its five largest containers, and the API server's resident memory and live heap, so a step
# that leaves a control plane short shows what grew. A server too pressed to answer in 10 s leaves
# a gap.
control_plane_record() {
    while :; do
        now=$(date -u +%T)
        for node in $(KUBECONFIG="$1" kubectl --request-timeout=10s get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[*].metadata.name}' 2> /dev/null); do
            KUBECONFIG="$1" kubectl --request-timeout=10s get --raw "/api/v1/nodes/${node}/proxy/stats/summary" 2> /dev/null |
                jq -r --arg t "${now}" '"\($t) \(.node.nodeName): \(.node.memory.workingSetBytes / 1048576 | floor) MiB in use; " +
                    ([.pods[].containers[] | {name, mib: ((.memory.workingSetBytes // 0) / 1048576 | floor)}] | sort_by(-.mib) | .[:5] | map("\(.name) \(.mib)") | join(", "))' || true
        done
        KUBECONFIG="$1" kubectl --request-timeout=10s get --raw /metrics 2> /dev/null |
            awk -v t="${now}" '/^process_resident_memory_bytes / { r = $2 } /^go_memstats_heap_inuse_bytes / { h = $2 } /^go_memstats_next_gc_bytes / { g = $2 }
                END { if (r) printf "%s API server: %d MiB resident, %d MiB of heap in use, next collection at %d MiB\n", t, r / 1048576, h / 1048576, g / 1048576 }' || true
        sleep 15
    done
}

# absent <kubectl get command...>: whether the API server answers that the object does not exist.
# Any other failure, an unreachable server among them, is not taken for absence.
absent() {
    if out=$("$@" -o name 2>&1); then
        return 1
    fi
    case ${out} in
        *"(NotFound)"*) ;;
        *) return 1 ;;
    esac
}
