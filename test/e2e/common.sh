#!/bin/sh
# Helpers every end-to-end test uses. Sourced after `set -eu`.

# The appliance tests' helpers below read variables each test sets, and set $csrf for it.
# shellcheck disable=SC2154,SC2034

# The images the tests run their own pods from.
# renovate: datasource=docker depName=registry.k8s.io/e2e-test-images/agnhost
agnhost=registry.k8s.io/e2e-test-images/agnhost:2.66.1@sha256:6e2eef87d0dc77e070a7549bc5fdfa290c0fefe3359eb33c2ad1eda692fe66a4
# renovate: datasource=docker depName=registry.k8s.io/pause
pause_image=registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
# The Git server the GitOps engines sync from, rootless as Pod Security's restricted level needs.
# renovate: datasource=docker depName=docker.io/gitea/gitea
gitea=docker.io/gitea/gitea:28.3.0-rootless@sha256:40c5a68a0463c0940e1ae32626c8300dd0572f3558e390073ad8379ca67c0e1d
export agnhost pause_image gitea
# What the test's own pods request, in MiB. The manifests and the worker's sizing both read these.
# Every agnhost pod workload-cluster.sh creates is listed as <namespace>/<name> in one of two lists,
# which test/unit/lane-sizing.sh enforces. The first runs while the GitOps steps do, so a worker is
# sized for it. The second is deleted before them.
gitops_fixture_pods='e2e-lb/web e2e-mesh/server e2e-mesh/client e2e-plain/client e2e-git/client'
# shellcheck disable=SC2034 # read by test/unit/lane-sizing.sh.
earlier_fixture_pods='e2e-policy/server e2e-policy/client e2e-health/crasher e2e-ingress/web e2e-dual/web e2e-dual/client e2e-certs/web e2e-acme/client'
agnhost_request_mib=16
gitea_request_mib=128
# word_count <words...>
word_count() {
    echo "$#"
}
# shellcheck disable=SC2086 # one pod per word.
gitops_agnhost_pods=$(word_count ${gitops_fixture_pods})

# worker_fixtures <gitops: true|false> sets worker_memory_fixtures to what the test's own pods
# request on a worker, in MiB: with GitOps, Gitea and the agnhost pods the steps leave running.
worker_fixtures() {
    worker_memory_fixtures=0
    if [ "$1" = true ]; then
        worker_memory_fixtures=$((gitea_request_mib + gitops_agnhost_pods * agnhost_request_mib))
    fi
}

# worker_need_for <class JSON> <add-ons JSON array> <gitops: true|false>: what a worker uses itself
# with those add-ons on and the test's own pods, by Kubehenge's figures, in MiB.
worker_need_for() {
    worker_fixtures "$3"
    printf '%s' "$1" | jq -r --argjson on "$2" --argjson fixtures "${worker_memory_fixtures}" '
        (.metadata.annotations["kubehenge.dev/addon-memory"] | fromjson) as $addon
        | (.metadata.annotations["kubehenge.dev/node-memory"] | fromjson) as $node
        | $node.worker + ([$on[] | $addon[.]] | add) + $fixtures'
}

# worker_memory_for <class JSON> <add-ons JSON array> <gitops: true|false>: the least memory in MiB
# a worker may have for a class with those add-ons on. It is the largest of the class's own minimum,
# what a worker and the add-ons use, and what the scheduler needs for the requests of a worker's own
# pods, the add-ons' pods and, with GitOps, the test's own pods, to a multiple of 8.
worker_memory_for() {
    worker_fixtures "$3"
    printf '%s' "$1" | jq -r --argjson on "$2" --argjson fixtures "${worker_memory_fixtures}" '
        (.metadata.annotations["kubehenge.dev/addon-memory"] | fromjson) as $addon
        | (.metadata.annotations["kubehenge.dev/node-memory"] | fromjson) as $node
        | (.metadata.annotations["kubehenge.dev/addon-memory-requests"] | fromjson) as $requests
        | (.metadata.annotations["kubehenge.dev/node-memory-requests"] | fromjson) as $requested
        | [(.spec.variables[] | select(.name == "workerMachine") | .schema.openAPIV3Schema.properties.memoryMiB.minimum),
            $node.worker + ([$on[] | $addon[.]] | add) + $fixtures,
            (($requested.worker + ($requested.workers // 0) + ([$on[] | if . == "argocd" then ([$requests.argocd, $requests.flux] | max) else $requests[.] end] | add) + $fixtures) / 8 | ceil) * 8]
        | max'
}
# The S3 store the backup tests write to.
# renovate: datasource=github-release-attachments depName=seaweedfs/seaweedfs
SEAWEEDFS_VERSION=4.52
SEAWEEDFS_SHA256=8b538c22d47d0c3fb6792adaa6b0c735a967401270a53427d5a476dcaca9b7ae

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
# the long ones cost no more checks than a steady 10-second poll. A check that sets seen to what it
# found has its last finding in the failure, which a pod's log may not otherwise keep.
until_true() {
    limit=$(($(date +%s) + $1)) what=$2 pause=2 seen=
    shift 2
    until "$@"; do
        now=$(date +%s)
        [ "${now}" -lt "${limit}" ] || fail "timed out: ${what}${seen:+; last seen: ${seen}}"
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
    nodes=$(KUBECONFIG="$1" kubectl get nodes -o jsonpath='{.items[*].metadata.name}') || return 1
    for node in ${nodes}; do
        summary=$(KUBECONFIG="$1" kubectl get --raw "/api/v1/nodes/${node}/proxy/stats/summary") || return 1
        printf '%s' "${summary}" |
            jq -r --arg n "${node}" '.node.memory | "\($n): \(.workingSetBytes / 1048576 | floor) MiB of memory in use, \(.availableBytes / 1048576 | floor) MiB available"' || return 1
        pods=$(printf '%s' "${summary}" | jq -c --argjson all "${pods}" '$all + [.pods[] | {
            ns: (if .podRef.namespace == "kube-system" and (.podRef.name | startswith("kubehenge-")) then "kube-system (Kubehenge metrics proxies)" else .podRef.namespace end),
            name: .podRef.name, bytes: (.memory.workingSetBytes // 0)}]') || return 1
    done
    printf '%s' "${pods}" | jq -r 'group_by(.ns)[] | "  \(.[0].ns): \(map(.bytes) | add / 1048576 | floor) MiB in \(length) pods"' || return 1
    printf '%s' "${pods}" | jq -r '.[] | select(.ns == "metallb-system" or (.ns == "kube-system" and (.name | startswith("kube-vip")))) |
        "    \(.name): \(.bytes / 1048576 | floor) MiB"'
}

# control_plane_use <kubeconfig> <add-on>: each control plane's memory in use once the add-on is
# on, from its kubelet's working set. From one add-on to the next it grows by what the class's
# kubehenge.dev/addon-control-plane-memory figures size.
control_plane_use() {
    nodes=$(KUBECONFIG="$1" kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[*].metadata.name}') || return 1
    for node in ${nodes}; do
        summary=$(KUBECONFIG="$1" kubectl get --raw "/api/v1/nodes/${node}/proxy/stats/summary") || return 1
        printf '%s' "${summary}" |
            jq -r --arg n "${node}" --arg a "$2" '"with \($a): \($n) has \(.node.memory.workingSetBytes / 1048576 | floor) MiB of memory in use"' || return 1
    done
}

# measured <heading> <command>...: a measurement's figures, under the heading when there is one.
# A reading that fails is tried again every KUBEHENGE_E2E_MEASURE_EVERY seconds (10) for
# KUBEHENGE_E2E_MEASURE_TIMEOUT seconds (120), as a kubelet answers only once its serving
# certificate is back. After that the measurement is named unavailable with the reading's last
# error, and the test goes on: a measurement never fails a test.
measured() {
    measured_heading=$1
    shift
    measured_limit=$(($(date +%s) + ${KUBEHENGE_E2E_MEASURE_TIMEOUT:-120}))
    while :; do
        # shellcheck disable=SC2310 # a failed reading is only a retry.
        if measured_out=$("$@" 2>&1); then
            [ -z "${measured_heading}" ] || printf '%s:\n' "${measured_heading}"
            printf '%s\n' "${measured_out}"
            return 0
        fi
        measured_now=$(date +%s)
        [ "${measured_now}" -lt "${measured_limit}" ] || break
        sleep "${KUBEHENGE_E2E_MEASURE_EVERY:-10}"
    done
    measured_why=$(printf '%s\n' "${measured_out}" | grep -v '^ *$' | tail -n 1)
    printf '%s: unavailable after %d s: %s\n' "${measured_heading:-$1}" "${KUBEHENGE_E2E_MEASURE_TIMEOUT:-120}" "${measured_why:-no output}"
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

# flux_ready <kubeconfig>: whether the Flux Operator reports its FluxInstance ready. Until then seen
# holds the Ready condition, whose message names the component or sync object the operator waits for.
flux_ready() {
    flux_instance=$(KUBECONFIG="$1" kubectl -n flux-system get fluxinstance flux -o json 2>&1) || {
        seen=$(printf '%s\n' "${flux_instance}" | tail -n 1 | cut -c1-400)
        return 1
    }
    flux_condition=$(printf '%s' "${flux_instance}" | jq -j '[.status.conditions[]? | select(.type == "Ready")][0] //
        {status: "Unknown", message: "no Ready condition yet"} | "\(.status) \(.reason // "-"): \(.message // "" | gsub("\\s+"; " "))"' |
        cut -c1-400) || return 1
    case ${flux_condition} in
        "True "*) return 0 ;;
        *) seen=${flux_condition} ;;
    esac
    return 1
}

# flux_state <kubeconfig>: what holds a FluxInstance up in a failed test, which no collected log
# keeps: its conditions, the sync's GitRepository and Kustomization, each pod in flux-system with
# the conditions it does not meet and how its containers last ended, the namespace's events and the
# operator's last lines. A cluster without Flux prints nothing.
flux_state() {
    KUBECONFIG="$1" kubectl get namespace flux-system > /dev/null 2>&1 || return 0
    printf '\nFlux:\n'
    if flux_instance=$(KUBECONFIG="$1" kubectl -n flux-system get fluxinstance flux -o json 2>&1); then
        printf '%s' "${flux_instance}" | jq -r '.status.conditions[]? | "FluxInstance \(.type)=\(.status) \(.reason // "-"): \(.message // "")"' || true
    else
        printf 'FluxInstance: %s\n' "${flux_instance}"
    fi
    KUBECONFIG="$1" kubectl -n flux-system get gitrepositories,kustomizations 2>&1 || true
    if flux_pods=$(KUBECONFIG="$1" kubectl -n flux-system get pods -o json 2>&1); then
        # A pending pod's PodScheduled says what no node had room for; a waiting container says
        # why, such as an image pull backing off; a container killed for memory ended with 137.
        printf '%s' "${flux_pods}" | jq -r '.items[] |
            "\(.metadata.name) on \(.spec.nodeName // "no node"): \(.status.phase)",
            (.status.conditions[]? | select(.status != "True") | "  \(.type) \(.reason // "-"): \(.message // "")"),
            ((.status.initContainerStatuses // []) + (.status.containerStatuses // []) | .[] |
                "  \(.name): \(.state | keys[0])" +
                (.state[] | (if .reason then " " + .reason else "" end) + (if .message then " (" + .message[:200] + ")" else "" end)) +
                ", ready \(.ready), \(.restartCount) restarts" +
                (if .lastState.terminated then ", last ended \(.lastState.terminated.reason // "-") with exit \(.lastState.terminated.exitCode) at \(.lastState.terminated.finishedAt // "-")" else "" end))' || true
    else
        printf 'pods: %s\n' "${flux_pods}"
    fi
    KUBECONFIG="$1" kubectl -n flux-system get events --sort-by=.metadata.creationTimestamp 2>&1 | tail -n 40 || true
    KUBECONFIG="$1" kubectl -n flux-system logs deployment/flux-operator --tail=40 2>&1 || true
}

# seaweedfs <directory>: downloads SeaweedFS's weed into the directory, checked by its SHA-256.
seaweedfs() {
    "$(dirname "$0")/../../scripts/fetch.sh" \
        "https://github.com/seaweedfs/seaweedfs/releases/download/${SEAWEEDFS_VERSION}/linux_amd64.tar.gz" "$1/seaweedfs.tar.gz"
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

# seaweedfs_stop <PID>: stops the weed seaweedfs_server ran, a child of this shell, and returns once
# it is gone. Its ports stay taken while it shuts down, so the next test of a lane, starting its own
# store seconds later, would find them in use. A weed still there after
# KUBEHENGE_E2E_S3_STOP_TIMEOUT seconds (30) is killed.
seaweedfs_stop() {
    kill "$1" 2> /dev/null || return 0
    # Its sleep outlives the watchdog killed below, and holds none of the test's output open.
    (sleep "${KUBEHENGE_E2E_S3_STOP_TIMEOUT:-30}" && kill -9 "$1") > /dev/null 2>&1 &
    stop_watchdog=$!
    wait "$1" 2> /dev/null || true
    kill "${stop_watchdog}" 2> /dev/null || true
}

# seaweedfs_fatal <directory>: the fatal error seaweedfs_server's weed stopped on there, if it did.
seaweedfs_fatal() {
    [ -f "$1/s3.log" ] || return 1
    grep -E '^F[0-9]{4} ' "$1/s3.log" | tail -n 1 | grep .
}

# seaweedfs_answers <directory> <command...>: runs the command against the store seaweedfs_server
# runs from the directory, for until_true, and fails the test at once if its weed stopped on a fatal
# error, which no retry mends.
seaweedfs_answers() {
    if fatal=$(seaweedfs_fatal "$1"); then
        fail "the S3 store stopped: ${fatal}"
    fi
    shift
    "$@"
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

# The appliance tests' helpers (appliance.sh, appliance-upgrade.sh). They read variables the test
# sets before it calls them: $work (the scratch directory), $ca (the cluster's CA), $ui (the
# interface's URL), $ctid and $address (the appliance's container and its address with prefix),
# $install (the installer under test), $password and $passphrase, $admin and $admin_csrf, and the
# KUBEHENGE_E2E_* settings install_appliance names. Answers go to $work/status and $work/body, and
# to $csrf, $got and $upgraded.

# request <cookie jar> <curl arguments...>: a request to the appliance; its status goes to
# $work/status and its body to $work/body.
request() {
    jar=$1
    shift
    curl -sS --cacert "${ca}" -b "${jar}" -c "${jar}" -o "${work}/body" -w '%{http_code}' "$@" > "${work}/status"
}

# post <cookie jar> <CSRF token> <URL> <JSON>, and put and delete the same
post() {
    request "$1" -H 'Content-Type: application/json' -H "X-Kubehenge-CSRF: $2" -d "$4" "$3"
}
put() {
    request "$1" -X PUT -H 'Content-Type: application/json' -H "X-Kubehenge-CSRF: $2" -d "$4" "$3"
}
delete() {
    request "$1" -X DELETE -H "X-Kubehenge-CSRF: $2" "$3"
}

# expect <status> <what> <command...>: runs a request and checks the status it got.
expect() {
    want=$1 what=$2
    shift 2
    "$@"
    got=$(cat "${work}/status")
    [ "${got}" = "${want}" ] || fail_with_body "${what}: ${got} instead of ${want}"
}

# fail_with_body <message>: fails with the start of the last answer's body.
fail_with_body() {
    body=$(head -c 500 "${work}/body")
    fail "$1: ${body}"
}

# body_is <jq filter> <message>: checks the last answer's body.
body_is() {
    jq -e "$1" "${work}/body" > /dev/null || fail_with_body "$2"
}

# body_has <message> <jq arguments...>: body_is with a filter that takes arguments.
body_has() {
    message=$1
    shift
    jq -e "$@" "${work}/body" > /dev/null || fail_with_body "${message}"
}

# answers: whether the interface is up again, on its certificate.
answers() {
    curl -sS -o /dev/null --cacert "${ca}" "${ui}/api/realms" 2> /dev/null
}

# sign_in <cookie jar> <user>: signs in with the test's password, the session in the jar and its
# CSRF token in $csrf.
sign_in() {
    body=$(jq -n --arg u "$2" --arg p "${password}" '{username: $u, password: $p}')
    expect 200 "signing in as $2" request "$1" -H 'Content-Type: application/json' -d "${body}" "${ui}/api/session"
    csrf=$(jq -r .csrf "${work}/body")
}

# setup_status: the setup's status as the administrator sees it, in $work/body.
setup_status() {
    expect 200 "the setup's status" request "${work}/admin.jar" "${ui}/api/setup"
}

# job_ended: whether the appliance's last job has ended.
job_ended() {
    setup_status
    jq -e '.job.state != "running"' "${work}/body" > /dev/null
}

# job_was <kind> <state> <ready>: checks how the last job ended and whether the management cluster
# is ready after it, with the job's log when not as expected.
job_was() {
    if ! jq -e --arg k "$1" --arg s "$2" --argjson r "$3" '.job.kind == $k and .job.state == $s and .ready == $r' "${work}/body" > /dev/null; then
        jq -r '"kind \(.job.kind), state \(.job.state), ready \(.ready): \(.job.error // "")", (.job.log // [] | .[])' "${work}/body" >&2
        fail "the $1 job did not end $2 with the management cluster ready: $3"
    fi
}

# ready_again: whether the interface, after a restart, found the management cluster again.
ready_again() {
    setup_status
    jq -e .ready "${work}/body" > /dev/null
}

# install_appliance <installer> <release>: installs the appliance from the release archive with the
# installer, which is the one the release itself was published with when the test installs a
# published release as a user does. The container is KUBEHENGE_E2E_APPLIANCE_ID at
# KUBEHENGE_E2E_APPLIANCE_ADDRESS on KUBEHENGE_E2E_BRIDGE (default vmbr0), and its disk goes on
# KUBEHENGE_E2E_STORAGE when that is set.
install_appliance() {
    installer=$1 archive=$2
    sum=$(sha256sum "${archive}" | cut -d ' ' -f 1)
    set -- --ctid "${ctid}" --ip "${address}" --gateway "${KUBEHENGE_E2E_GATEWAY}" --dns "${KUBEHENGE_E2E_DNS}" \
        --bridge "${KUBEHENGE_E2E_BRIDGE:-vmbr0}" --hostname kubehenge-e2e --release "${archive}" --sha256 "${sum}" --no-pve-entry
    [ -z "${KUBEHENGE_E2E_STORAGE:-}" ] || set -- "$@" --storage "${KUBEHENGE_E2E_STORAGE}"
    if ! sh "${installer}" "$@" < /dev/null > "${work}/install.log" 2>&1; then
        cat "${work}/install.log" >&2
        fail "install.sh failed"
    fi
    grep -qF "Kubehenge is running in container ${ctid}: ${ui}/" "${work}/install.log" ||
        fail "install.sh did not name the appliance's address"
}

# upgrade <release>: install.sh --upgrade of the appliance, its exit status in $upgraded and its
# output in $work/upgrade.log.
upgrade() {
    sum=$(sha256sum "$1" | cut -d ' ' -f 1)
    upgraded=0
    sh "${install}" --upgrade --ctid "${ctid}" --release "$1" --sha256 "${sum}" < /dev/null > "${work}/upgrade.log" 2>&1 || upgraded=$?
}

# upgrade_refused <release> <reason>: checks that install.sh --upgrade stops, giving the reason.
upgrade_refused() {
    upgrade "$1"
    if [ "${upgraded}" -eq 0 ] || ! grep -q "$2" "${work}/upgrade.log"; then
        cat "${work}/upgrade.log" >&2
        fail "install.sh --upgrade to $1 did not stop with: $2"
    fi
}

# version_of <release>: the version of a release archive.
version_of() { tar -xzOf "$1" ./share/kubehenge/VERSION; }

# runs <version>: checks that the appliance runs that release, its interface answering.
runs() {
    installed=$(pct exec "${ctid}" -- cat /usr/local/share/kubehenge/VERSION)
    [ "${installed}" = "$1" ] || fail "the appliance runs ${installed} instead of $1"
    until_true 120 "the interface to answer" answers
}

# list <jq filter> <pvesh get arguments...>: an answer of Proxmox VE's API as lines. Through a file
# rather than a pipe, so that a query that fails stops the test instead of leaving lines out.
list() {
    filter=$1
    shift
    pvesh get "$@" --output-format json < /dev/null > "${work}/answer"
    jq -r "${filter}" "${work}/answer"
}

# inventory: what Proxmox VE holds that the appliance or the management cluster could leave behind,
# a line each. Container templates stay: install.sh keeps the one it downloaded for the next install.
inventory() {
    node=$(hostname)
    list '.[] | select(.active == 1) | .storage' "/nodes/${node}/storage" --enabled 1 > "${work}/storages"
    {
        list '.[] | "guest \(.vmid) \(.name // "")"' /cluster/resources --type vm
        list '.[] | "pool \(.poolid)"' /pools
        list '.[] | "user \(.userid) \(.tokens // [] | map(.tokenid) | sort | join(","))"' /access/users --full 1
        list '.[] | "group \(.groupid)"' /access/groups
        list '.[] | "role \(.roleid)"' /access/roles
        list '.[] | "acl \(.path) \(.type) \(.ugid) \(.roleid)"' /access/acl
        list '.[] | "ha resource \(.sid)"' /cluster/ha/resources
        list '.[] | "ha rule \(.rule)"' /cluster/ha/rules
        while read -r storage; do
            list '.[] | select(.content != "vztmpl") | "volume \(.volid)"' "/nodes/${node}/storage/${storage}/content"
        done < "${work}/storages"
    } > "${work}/inventory"
    sort "${work}/inventory"
}

# management_credentials: fetches the management cluster's admin kubeconfig through the interface
# and keeps its certificates in $work for management_get; $management_api is the cluster's API
# address.
management_credentials() {
    asked=$(jq -nc --arg p "${passphrase}" '{name: "kubeconfig", passphrase: $p}')
    expect 200 "the management cluster's admin kubeconfig" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup/credential" "${asked}"
    cp "${work}/body" "${work}/management.kubeconfig"
    kubeconfig_field certificate-authority-data | base64 -d > "${work}/management-ca.crt"
    kubeconfig_field client-certificate-data | base64 -d > "${work}/management.crt"
    kubeconfig_field client-key-data | base64 -d > "${work}/management.key"
    management_api=$(kubeconfig_field server)
}
kubeconfig_field() {
    sed -n "s/^ *$1: *//p" "${work}/management.kubeconfig" | head -n 1
}

# management_get <path>: a read of the management cluster's API with the admin kubeconfig.
management_get() {
    curl -sS --cacert "${work}/management-ca.crt" --cert "${work}/management.crt" --key "${work}/management.key" "${management_api}$1"
}
