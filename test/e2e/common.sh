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
# The S3 store the backup tests write to.
# renovate: datasource=github-release-attachments depName=seaweedfs/seaweedfs
SEAWEEDFS_VERSION=4.51
SEAWEEDFS_SHA256=c03be37416a8ee672c77766e20b22a4e1430744c3af2668599a4b6eeaec14cee

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

# pod_cpu <kubeconfig>: the CPU each node's pods request of what the node offers them, then the CPU
# the pods of each namespace request over all nodes, as the scheduler counts a pod: its containers
# and sidecars together, or its largest other init container, whichever is more. The add-ons' CPU
# figures are these requests, so the print shows one that fell behind its chart.
pod_cpu() {
    # Both lists go to jq on its input: a cluster's pods outgrow what one argument may hold.
    {
        KUBECONFIG="$1" kubectl get nodes -o json &&
            KUBECONFIG="$1" kubectl get pods -A --field-selector=status.phase!=Succeeded,status.phase!=Failed -o json
    } | jq -rs '
        def milli: if . == null then 0 elif endswith("m") then .[:-1] | tonumber else tonumber * 1000 end;
        def req: (.resources.requests.cpu // .resources.limits.cpu) | milli;
        .[0] as $nodes
        | (.[1].items | map({node: .spec.nodeName, ns: .metadata.namespace, cpu: ([
            ([.spec.containers[], (.spec.initContainers[]? | select(.restartPolicy == "Always")) | req] | add),
            ([.spec.initContainers[]? | select(.restartPolicy != "Always") | req] | max // 0)
        ] | max)})) as $p
        | ($nodes.items[] | .metadata.name as $n
            | "\($n): \($p | map(select(.node == $n) | .cpu) | add // 0)m of CPU requested of \(.status.allocatable.cpu | milli)m"),
        ($p | group_by(.ns)[] | "  \(.[0].ns): \(map(.cpu) | add)m in \(length) pods")'
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

# seaweedfs <directory>: downloads SeaweedFS's weed into the directory, checked by its SHA-256.
seaweedfs() {
    curl -sS --fail -L -o "$1/seaweedfs.tar.gz" \
        "https://github.com/seaweedfs/seaweedfs/releases/download/${SEAWEEDFS_VERSION}/linux_amd64.tar.gz"
    printf '%s  %s\n' "${SEAWEEDFS_SHA256}" "$1/seaweedfs.tar.gz" | sha256sum -c - > /dev/null ||
        fail "SeaweedFS's download does not have its SHA-256"
    tar -xzf "$1/seaweedfs.tar.gz" -C "$1" weed
}

# seaweedfs_server <directory> <S3 port> <S3 identities> <certificate> <key>: runs the weed that
# seaweedfs fetched into the directory in the background, as a server with an S3 gateway, its data
# in <directory>/s3-data and its log in <directory>/s3.log.
# SeaweedFS's default of 8 volume slots is too few: the filer's own log takes 7 of them once the
# server has run a minute, leaving a bucket written after that a single volume, and a write that
# would overfill it fails with InternalError. Slots cost no disk until a volume is written.
# The embedded IAM API, which the tests do not use, fails to start without an STS signing key and
# says so as an error; the identities hold either way.
seaweedfs_server() {
    mkdir -p "$1/s3-data"
    "$1/weed" server -dir="$1/s3-data" -ip=127.0.0.1 -ip.bind=0.0.0.0 -master.port=19333 \
        -master.volumeSizeLimitMB=64 -volume.max=100 -volume.port=18080 -filer.port=18888 \
        -s3 -s3.port="$2" -s3.config="$3" -s3.iam=false -s3.cert.file="$4" -s3.key.file="$5" > "$1/s3.log" 2>&1 &
}

# seaweedfs_errors <directory>: the last errors seaweedfs_server logged there, if it ran, since a
# client sees only InternalError for most of them.
seaweedfs_errors() {
    [ -f "$1/s3.log" ] || return 0
    echo "-- the S3 store's last errors" >&2
    grep -E '^[EF][0-9]{4} ' "$1/s3.log" | tail -n 20 >&2 || true
}

# boot_logs <namespace> <Cluster> <Machine...>: asks Kubehenge's controller for the machines' boot
# logs as the interface and kubectl alike ask, by naming them in the Cluster's
# kubehenge.dev/boot-log-request annotation, waits until it has taken every name off, and prints
# what it read, the entries of the ConfigMap <Cluster>-boot-logs as one JSON object by machine.
boot_logs() {
    logs_ns=$1 logs_cluster=$2
    shift 2
    logs_asked=$(printf '%s,' "$@")
    kubectl -n "${logs_ns}" annotate --overwrite "cluster/${logs_cluster}" "kubehenge.dev/boot-log-request=${logs_asked%,}" > /dev/null
    until_true 600 "the controller reads the boot logs of $*" boot_logs_answered
    kubectl -n "${logs_ns}" get configmap "${logs_cluster}-boot-logs" -o json | jq -c '.data | map_values(fromjson)'
}
boot_logs_answered() {
    kubectl -n "${logs_ns}" get "cluster/${logs_cluster}" -o json |
        jq -e '.metadata.annotations["kubehenge.dev/boot-log-request"] // "" | . == ""' > /dev/null
}

# boot_log_shows <logs> <Machine> <log> <pattern>: whether the machine's boot log, as boot_logs
# printed it, holds the log of that name read without an error, with a line matching the pattern (a
# jq regular expression).
boot_log_shows() {
    printf '%s' "$1" | jq -e --arg m "$2" --arg s "$3" --arg re "$4" '.[$m] | (.error // "") == "" and
        any(.sections[]?; .name == $s and (.error // "") == "" and any(.lines[]?; test($re)))' > /dev/null
}

# boot_log_summary <logs> <Machine>: what was read of the machine's boot log, each log's name, error
# and last lines, on standard error.
boot_log_summary() {
    printf '%s' "$1" | jq -r --arg m "$2" '.[$m] // {error: "not read"} | "boot log of \($m): \(.error // "read")",
        (.sections[]? | "--- \(.name): \(.error // "read"), \(.lines // [] | length) lines", (.lines // [] | .[-5:][] | "    " + .))' >&2
}
