#!/bin/sh
# End-to-end test of the weekly template refresh on a Proxmox VE host, against a running management
# cluster whose refresh is on with no soak, k3s's template on the default node OS built once, and
# on Rocky Linux 10 too: a one-node k3s cluster runs that build; a run of the refresh, asked for as
# Build now asks, makes a new one; the cluster's node stays on its build while the maintenance window
# is closed and moves to the new one once it opens; a second run makes a third build and deletes the
# first, which nothing runs any more, while the two newest stay; the cluster's node OS changed to
# Rocky Linux replaces its node with a clone of that template, SELinux enforcing on it and its boot
# log read through its guest agent; nothing of
# the cluster is left afterwards.
#
#   test/e2e/node-image-refresh.sh
#
# KUBECONFIG points at the management cluster. KUBEHENGE_PROXMOX_URL and KUBEHENGE_PROXMOX_TOKEN
# reach Proxmox VE with rights to inspect everything; KUBEHENGE_PROXMOX_CA is the API's CA if the
# system does not trust it. Network and storage, as for workload-cluster.sh: KUBEHENGE_E2E_BRIDGE,
# KUBEHENGE_E2E_PREFIX, KUBEHENGE_E2E_GATEWAY, KUBEHENGE_E2E_DNS, KUBEHENGE_E2E_STORAGE,
# KUBEHENGE_E2E_ADDRESSES and KUBEHENGE_E2E_ENDPOINT. KUBEHENGE_E2E_VERSION is the k3s version;
# KUBEHENGE_E2E_CORES and _MEMORY (MiB) size the node, by default the least of a k3s control plane
# that runs the workloads too; KUBEHENGE_E2E_POOL is the template pool, kubehenge-templates unless
# set.
set -eu

: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_STORAGE:?}"
: "${KUBEHENGE_E2E_ADDRESSES:?}" "${KUBEHENGE_E2E_ENDPOINT:?}" "${KUBEHENGE_E2E_VERSION:?}"
pool=${KUBEHENGE_E2E_POOL:-kubehenge-templates}

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

ns=e2e-refresh-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
cleanup() {
    status=$?
    if [ "${status}" -ne 0 ]; then
        printf '\nleaving the cluster in %s for inspection; delete it with: kubectl delete namespace %s\n' "${ns}" "${ns}"
    fi
}
trap cleanup EXIT

curl_ca=
[ -n "${KUBEHENGE_PROXMOX_CA:-}" ] && curl_ca="--cacert ${KUBEHENGE_PROXMOX_CA}"
# shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
pve_get() { curl -sS --fail ${curl_ca} -H "Authorization: PVEAPIToken=${KUBEHENGE_PROXMOX_TOKEN}" "${KUBEHENGE_PROXMOX_URL}/api2/json/$1"; }
json_list() { printf '%s' "$1" | jq -R 'split(",")'; }
system() { kubectl -n kubehenge-system "$@"; }

# builds [node OS]: the build tags of k3s's templates on the node OS, by default the class's, in the
# template pool, oldest first.
default_os=$(kubectl -n kubehenge-system get clusterclass kubehenge-k3s -o json |
    jq -r '.spec.variables[] | select(.name == "nodeOS") | .schema.openAPIV3Schema.default | "\(.family)-\(.release)"')
builds() {
    pve_get cluster/resources?type=vm |
        jq -r --arg p "${pool}" --arg os "${1:-${default_os}}" '[.data[] | select(.type == "qemu" and .template == 1 and .pool == $p)
            | .name | capture("^kubehenge-k3s-" + $os + "-(?<b>build-[0-9]{12})$").b] | sort[]'
}
builds_are() {
    got=$(builds)
    got=$(printf '%s\n' "${got}" | tr '\n' ' ')
    [ "${got}" = "$1" ]
}
machine_name() { kubectl -n "${ns}" get machines -o jsonpath='{.items[0].metadata.name}'; }

# offered <build>: whether the controller lists the build to the k3s class, which is when clusters
# may move to it.
offered() {
    at=$(printf '%s' "$1" | sed 's/^build-\(....\)\(..\)\(..\)\(..\)\(..\)$/\1-\2-\3T\4:\5:00Z/')
    system get clusterclass/kubehenge-k3s -o json |
        jq -e --arg at "${at}" '.metadata.annotations["kubehenge.dev/node-image-builds"] // "{}" | fromjson | [.[] | index($at)] | any' > /dev/null
}

# runs <build>: whether the cluster's machines, all of them ready, are clones of the build.
runs() {
    kubectl -n "${ns}" get proxmoxmachines -o json |
        jq -e --arg b "$1" '.items | length > 0 and all(.spec.templateSelector.matchTags | index($b))' > /dev/null &&
        kubectl -n "${ns}" get machines -o json | jq -e '.items | length == 1 and all(.status.nodeRef != null and
            ([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length == 1))' > /dev/null
}

# refresh: asks for a run of the refresh as the interface's Build now does, which the controller
# starts as a Job of the refresh's CronJob, and waits for that run to succeed.
refresh() {
    asked=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    request=$(jq -cn --arg at "${asked}" '{data: {refresh: ({by: "e2e", at: $at} | tojson)}}')
    system patch configmap kubehenge-template-requests --type=merge -p "${request}"
    until_true 300 "the controller starts the refresh" started_since "${asked}"
    until_true 5400 "the refresh finishes" finished
    run_log=$(system logs "job/${job}")
    printf '%s\n' "${run_log}" | tail -n 20
    state=$(system get configmap kubehenge-template-refresh -o jsonpath='{.data.state}')
    [ "${state}" = succeeded ] || fail "the refresh recorded its run as ${state}"
    # Each new build of k3s's template was seen, through Proxmox VE with the refresh's own token, to
    # hand out its boot log through its guest agent.
    printf '%s\n' "${run_log}" | grep -q "hands out its boot log and cloud-init's output through the guest agent$" ||
        fail 'the refresh built no template that hands out its boot log'
}
started_since() {
    job=$(system get jobs -l kubehenge.dev/template-refresh=true -o json |
        jq -r --arg t "$1" '[.items[] | select(.metadata.creationTimestamp >= $t)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty')
    [ -n "${job}" ]
}
finished() {
    conditions=$(system get "job/${job}" -o json | jq -r '[.status.conditions[]? | select(.status == "True") | .type] | join(" ")')
    case " ${conditions} " in
        *" Complete "*) return 0 ;;
        *" Failed "*)
            kubectl -n kubehenge-system logs "job/${job}" >&2 || true
            fail "the refresh ${job} failed"
            ;;
        *) return 1 ;;
    esac
}

# held_from <build>: whether the controller has decided to hold the cluster from the build until its
# window opens, which it records on the cluster when it looks at it with the build offered.
held_from() {
    held=$(kubectl -n "${ns}" get cluster/node-image -o jsonpath='{.metadata.annotations.kubehenge\.dev/node-image-held}')
    case "${held}" in
        "$1 at "*) return 0 ;;
        *) return 1 ;;
    esac
}

# set_upgrades <window>: the cluster's upgrades variable, Kubernetes upgrades off, with the window.
set_upgrades() {
    vars=$(kubectl -n "${ns}" get cluster/node-image -o json |
        jq -c --argjson w "$1" '[.spec.topology.variables[] | select(.name != "upgrades")] + [{name: "upgrades", value: {mode: "off", window: $w}}]')
    kubectl -n "${ns}" patch cluster/node-image --type=merge -p "{\"spec\":{\"topology\":{\"variables\":${vars}}}}"
}

step "the first build of k3s's template on ${default_os}"
first=$(builds)
case "${first}" in
    *[[:space:]]* | "") fail "the template pool holds the builds '${first}', not one" ;;
    *) echo "${first}" ;;
esac

step "a one-node k3s cluster on it, its window closed"
kubectl create namespace "${ns}"
# Twelve hours from now, so the window is closed for the whole test.
hour=$(date -u +%H)
hour=${hour#0}
start=$(printf '%02d:00' $(((hour + 12) % 24)))
closed=$(jq -cn --arg s "${start}" '{start: $s, durationMinutes: 30, timeZone: "UTC"}')
addresses=$(json_list "${KUBEHENGE_E2E_ADDRESSES}")
dns=$(json_list "${KUBEHENGE_E2E_DNS}")
kubectl apply -f - << EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: node-image
  namespace: ${ns}
spec:
  topology:
    classRef:
      name: kubehenge-k3s
      namespace: kubehenge-system
    version: ${KUBEHENGE_E2E_VERSION}
    controlPlane:
      replicas: 1
    workers:
      machineDeployments:
        - class: worker
          name: md-0
          replicas: 0
    variables:
      - name: controlPlaneEndpoint
        value: ${KUBEHENGE_E2E_ENDPOINT}
      - name: network
        value:
          bridge: ${KUBEHENGE_E2E_BRIDGE}
          addresses: ${addresses}
          prefix: ${KUBEHENGE_E2E_PREFIX}
          gateway: ${KUBEHENGE_E2E_GATEWAY}
          dnsServers: ${dns}
      - name: storage
        value: ${KUBEHENGE_E2E_STORAGE}
      - name: controlPlaneMachine
        value: {cores: ${KUBEHENGE_E2E_CORES:-3}, memoryMiB: ${KUBEHENGE_E2E_MEMORY:-5120}}
      - name: upgrades
        value: {mode: "off", window: ${closed}}
EOF
until_true 1800 "the cluster runs ${first}" runs "${first}"
machine=$(machine_name)

step "a run of the refresh makes a new build"
refresh
second=$(builds | tail -n 1)
[ "${second}" != "${first}" ] || fail 'the refresh made no new build'
until_true 120 "both builds are there" builds_are "${first} ${second} "
until_true 120 "the controller offers ${second}" offered "${second}"

step "the controller holds the node on its build while the window is closed"
until_true 600 "the controller holds the cluster from ${second} for its window" held_from "${second}"
kubectl -n "${ns}" get cluster/node-image -o jsonpath='{.metadata.annotations.kubehenge\.dev/node-image-held}'
echo
# shellcheck disable=SC2310 # runs only reads, and its failure is the answer looked for.
runs "${first}" || fail 'the node moved while the window was closed'
now=$(machine_name)
[ "${now}" = "${machine}" ] || fail 'the node was replaced while the window was closed'

step "the node moves to the new build once the window opens"
set_upgrades '{"start": "00:00", "durationMinutes": 1440, "timeZone": "UTC"}'
until_true 2400 "the cluster runs ${second}" runs "${second}"
now=$(machine_name)
[ "${now}" != "${machine}" ] || fail 'the node runs the new build without having been replaced'
refreshed=$(kubectl -n "${ns}" get cluster/node-image -o jsonpath='{.metadata.annotations.kubehenge\.dev/node-image-refreshed}')
case "${refreshed}" in
    "${first} to ${second} at "*) echo "${refreshed}" ;;
    *) fail "the cluster records its last refresh as '${refreshed}'" ;;
esac
# shellcheck disable=SC2310 # held_from only reads, and its failure is the answer looked for.
! held_from "${second}" || fail 'the cluster is still held from the build it moved to'

step "a second run makes a third build and deletes the first, which nothing runs"
# Back to closed, so that the node stays on the second build while the third is collected around it.
set_upgrades "${closed}"
refresh
third=$(builds | tail -n 1)
[ "${third}" != "${second}" ] || fail 'the second run made no new build'
until_true 120 "the two newest builds alone are there" builds_are "${second} ${third} "
# shellcheck disable=SC2310 # runs only reads, and its failure is the answer looked for.
runs "${second}" || fail 'the node left its build'

step "the node OS changed to Rocky Linux 10 replaces the node, which enforces SELinux"
rocky_builds=$(builds rocky-10)
[ -n "${rocky_builds}" ] || fail 'the template pool holds no build of k3s on Rocky Linux 10'
machine=$(machine_name)
vars=$(kubectl -n "${ns}" get cluster/node-image -o json |
    jq -c '[.spec.topology.variables[] | select(.name != "nodeOS")] + [{name: "nodeOS", value: {family: "rocky", release: "10"}}]')
kubectl -n "${ns}" patch cluster/node-image --type=merge -p "{\"spec\":{\"topology\":{\"variables\":${vars}}}}"
# A change of node OS is no node image refresh: the node moves at once, window closed or not.
until_true 2400 "the cluster runs a build of k3s on Rocky Linux 10" runs os-rocky-10
now=$(machine_name)
[ "${now}" != "${machine}" ] || fail 'the node runs Rocky Linux without having been replaced'
pm=$(kubectl -n "${ns}" get proxmoxmachines -o jsonpath='{.items[0].metadata.name}')
pm_node=$(kubectl -n "${ns}" get proxmoxmachine "${pm}" -o jsonpath='{.status.proxmoxNode}')
pm_vmid=$(kubectl -n "${ns}" get proxmoxmachine "${pm}" -o jsonpath='{.spec.virtualMachineID}')
cpu=$(pve_get "nodes/${pm_node}/qemu/${pm_vmid}/config" | jq -r .data.cpu)
case "${cpu}" in
    x86-64-v3* | host*) echo "CPU type ${cpu}" ;;
    *) fail "the Rocky Linux node runs on CPU type ${cpu}, below its x86-64-v3" ;;
esac
# Its boot log, read through its guest agent, whose SELinux policy the check below then sees as
# having denied nothing.
machine=$(machine_name)
boot_log=$(boot_logs "${ns}" node-image "${machine}")
for expected in "cloud-init-output.log|^Cloud-init v[.] " "journal of cloud-init|\[[0-9]+\]: " "journal of k3s|\[[0-9]+\]: "; do
    # shellcheck disable=SC2310 # its failure is reported below.
    if ! boot_log_shows "${boot_log}" "${machine}" "${expected%%|*}" "${expected#*|}"; then
        boot_log_summary "${boot_log}" "${machine}"
        fail "the boot log of the Rocky Linux node holds no ${expected%%|*}"
    fi
    echo "${expected%%|*}"
done
request=$(jq -cn '{command: ["sh", "-c", ". /etc/os-release; echo \"${ID} ${VERSION_ID} $(getenforce)\"; ! { journalctl -k -b --no-pager -q; cat /var/log/audit/audit.log 2>/dev/null; } | grep \"avc: *denied\""]}')
# shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
pid=$(curl -sS --fail ${curl_ca} -H "Authorization: PVEAPIToken=${KUBEHENGE_PROXMOX_TOKEN}" -H 'Content-Type: application/json' \
    -X POST -d "${request}" "${KUBEHENGE_PROXMOX_URL}/api2/json/nodes/${pm_node}/qemu/${pm_vmid}/agent/exec" | jq -r .data.pid)
# shellcheck disable=SC2310 # a failed read is a check that has not exited yet, asked again.
exited() { result=$(pve_get "nodes/${pm_node}/qemu/${pm_vmid}/agent/exec-status?pid=${pid}") && printf '%s' "${result}" | jq -e '.data.exited == 1' > /dev/null; }
until_true 60 "the check in the node exits" exited
out=$(printf '%s' "${result}" | jq -r '.data."out-data" // ""')
echo "${out}"
case "${out}" in
    "rocky 10."*" Enforcing"*) ;;
    *) fail "the node runs '${out}', not Rocky Linux 10 with SELinux enforcing" ;;
esac
printf '%s' "${result}" | jq -e '.data.exitcode == 0' > /dev/null || fail 'SELinux denied something on the node'

step "nothing of the cluster is left"
kubectl -n "${ns}" delete cluster/node-image --wait --timeout=30m
vms=$(pve_get cluster/resources?type=vm)
printf '%s' "${vms}" | jq -e --arg p "/${ns}." '[.data[] | select(.pool // "" | contains($p))] | length == 0' > /dev/null ||
    fail "VMs of the cluster are still there"
kubectl delete namespace "${ns}" --wait --timeout=5m

printf '\nPASS\n'
