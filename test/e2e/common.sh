#!/bin/sh
# Helpers every end-to-end test uses. Sourced after `set -eu`.

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
