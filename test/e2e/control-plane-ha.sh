#!/bin/sh
# End-to-end test of a highly available control plane on a Proxmox VE host, against a running
# management cluster: a k3s cluster's control plane grows from one server to three, and etcd's
# members with it. The server holding the cluster's virtual IP dies; the API stays reachable at that
# address from another server and takes writes, etcd keeping its quorum on the two left, until the
# control plane's health check has the dead server replaced. The control plane then shrinks back to
# one server, keeping what was written, and nothing of the cluster is left once it is deleted.
#
#   test/e2e/control-plane-ha.sh
#
# KUBECONFIG points at the management cluster. KUBEHENGE_PROXMOX_URL and KUBEHENGE_PROXMOX_TOKEN
# reach Proxmox VE with rights to inspect everything and to stop one of the cluster's VMs, standing
# in for a server that dies; KUBEHENGE_PROXMOX_CA is the API's CA if the system does not trust it.
# Network and storage, as for workload-cluster.sh: KUBEHENGE_E2E_BRIDGE, KUBEHENGE_E2E_PREFIX,
# KUBEHENGE_E2E_GATEWAY, KUBEHENGE_E2E_DNS, KUBEHENGE_E2E_STORAGE; the cluster's node addresses,
# at least six (three servers, a fourth while one is replaced, and a worker), and virtual IP:
# KUBEHENGE_E2E_ADDRESSES, KUBEHENGE_E2E_ENDPOINT. KUBEHENGE_E2E_VERSION is the k3s version.
# KUBEHENGE_E2E_CONTROL_PLANE_CORES (default 2) sizes the servers, each with the class's least
# memory, 4096 MiB; the worker has 2 cores and 1024 MiB.
set -eu

: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_STORAGE:?}"
: "${KUBEHENGE_E2E_ADDRESSES:?}" "${KUBEHENGE_E2E_ENDPOINT:?}" "${KUBEHENGE_E2E_VERSION:?}"

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

work=$(mktemp -d)
ns=e2e-ha-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
cleanup() {
    status=$?
    if [ "${status}" -ne 0 ]; then
        printf '\nleaving the cluster in %s for inspection; delete it with: kubectl delete namespace %s\n' "${ns}" "${ns}"
    fi
    rm -rf "${work}"
}
trap cleanup EXIT

curl_ca=
[ -n "${KUBEHENGE_PROXMOX_CA:-}" ] && curl_ca="--cacert ${KUBEHENGE_PROXMOX_CA}"
# shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
pve() { curl -sS --fail ${curl_ca} -H "Authorization: PVEAPIToken=${KUBEHENGE_PROXMOX_TOKEN}" "$@"; }
pve_get() { pve "${KUBEHENGE_PROXMOX_URL}/api2/json/$1"; }
json_list() { printf '%s' "$1" | jq -R 'split(",")'; }
# workload: kubectl in the cluster, through its virtual IP.
workload() { KUBECONFIG="${work}/ha" kubectl --request-timeout=20s "$@"; }

# vms: how many VMs the cluster's Proxmox VE pool holds.
vms() { pve_get "cluster/resources?type=vm" | jq --arg p "/${ns}." '[.data[] | select(.pool // "" | contains($p))] | length'; }
# servers <n>: whether the control plane has n servers, every one ready and up to date.
servers() {
    kubectl -n "${ns}" get cluster/ha -o json |
        jq -e --argjson n "$1" '.status.controlPlane | .desiredReplicas == $n and .replicas == $n and .readyReplicas == $n and .upToDateReplicas == $n' > /dev/null
}
# etcd_members <n>: whether n nodes are ready etcd members, as k3s labels the servers that hold one.
etcd_members() {
    # shellcheck disable=SC2310 # polled; a server that does not answer counts as no members.
    n=$(workload get nodes -l node-role.kubernetes.io/etcd=true -o json |
        jq '[.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length') || return 1
    [ "${n}" = "$1" ]
}
# vip_holder: the node kube-vip's leader election gave the cluster's virtual IP.
vip_holder() { workload -n kube-system get lease plndr-cp-lock -o jsonpath='{.spec.holderIdentity}'; }
# written <name>: writes a ConfigMap through the virtual IP, which etcd takes only with a quorum.
written() { workload -n default create configmap "$1" --from-literal=written=true > /dev/null 2>&1; }

step "a k3s cluster with one server"
kubectl create namespace "${ns}"
addresses=$(json_list "${KUBEHENGE_E2E_ADDRESSES}")
dns_servers=$(json_list "${KUBEHENGE_E2E_DNS}")
kubectl apply -f - << EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: ha
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
          replicas: 1
    variables:
      - name: controlPlaneEndpoint
        value: ${KUBEHENGE_E2E_ENDPOINT}
      - name: network
        value:
          bridge: ${KUBEHENGE_E2E_BRIDGE}
          addresses: ${addresses}
          prefix: ${KUBEHENGE_E2E_PREFIX}
          gateway: ${KUBEHENGE_E2E_GATEWAY}
          dnsServers: ${dns_servers}
      - name: storage
        value: ${KUBEHENGE_E2E_STORAGE}
      - name: controlPlaneMachine
        value: {cores: ${KUBEHENGE_E2E_CONTROL_PLANE_CORES:-2}, memoryMiB: 4096}
      - name: workerMachine
        value: {cores: 2, memoryMiB: 1024}
EOF
kubectl -n "${ns}" wait --for=condition=Available cluster/ha --timeout=40m
kubectl -n "${ns}" get secret ha-kubeconfig -o jsonpath='{.data.value}' | base64 -d > "${work}/ha"
until_true 600 "one server" servers 1
until_true 600 "one etcd member" etcd_members 1

step "the control plane grows to three servers, etcd with it"
kubectl -n "${ns}" patch cluster/ha --type=merge -p '{"spec":{"topology":{"controlPlane":{"replicas":3}}}}'
until_true 2400 "three servers ready" servers 3
until_true 600 "three etcd members" etcd_members 3
kubectl -n "${ns}" wait --for=condition=Available cluster/ha --timeout=10m
workload get nodes -o wide

step "the server holding the virtual IP dies; the API stays reachable and takes writes"
# The class's 300 s before a not-ready node counts as dead only adds waiting here; the integration
# tier pins the classes' own checks.
kubectl -n "${ns}" patch cluster/ha --type=json -p '[{"op": "add", "path": "/spec/topology/controlPlane/healthCheck", "value": {
  "checks": {"nodeStartupTimeoutSeconds": 1200, "unhealthyNodeConditions": [
    {"type": "Ready", "status": "Unknown", "timeoutSeconds": 60}, {"type": "Ready", "status": "False", "timeoutSeconds": 60}]}}}]'
until_true 120 "the control plane's health check at 60 s" sh -c "kubectl -n ${ns} get machinehealthchecks -o json |
    jq -e '[.items[] | select(.spec.selector.matchLabels | has(\"cluster.x-k8s.io/control-plane\"))
    | .spec.checks.unhealthyNodeConditions[].timeoutSeconds] | length > 0 and all(. == 60)' >/dev/null"
holder=$(vip_holder)
[ -n "${holder}" ] || fail "no server holds the virtual IP"
machine=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o json |
    jq -r --arg n "${holder}" '.items[] | select(.status.nodeRef.name == $n) | .metadata.name')
[ -n "${machine}" ] || fail "no server's machine has node ${holder}"
infra=$(kubectl -n "${ns}" get machine "${machine}" -o jsonpath='{.spec.infrastructureRef.name}')
vmid=$(kubectl -n "${ns}" get proxmoxmachine "${infra}" -o jsonpath='{.spec.virtualMachineID}')
node=$(kubectl -n "${ns}" get proxmoxmachine "${infra}" -o jsonpath='{.status.proxmoxNode}')
printf 'stopping VM %s (%s, node %s, holding the virtual IP) on %s\n' "${vmid}" "${machine}" "${holder}" "${node}"
pve -X POST "${KUBEHENGE_PROXMOX_URL}/api2/json/nodes/${node}/qemu/${vmid}/status/stop" > /dev/null
moved() {
    # shellcheck disable=SC2310 # polled; no answer is no move yet.
    now=$(vip_holder 2> /dev/null) || return 1
    [ -n "${now}" ] && [ "${now}" != "${holder}" ]
}
until_true 300 "another server holds the virtual IP" moved
until_true 300 "a write through the virtual IP with two of three etcd members" written ha-quorum

step "the dead server is replaced"
until_true 1800 "machine ${machine} replaced" absent kubectl -n "${ns}" get machine "${machine}"
until_true 2400 "three servers ready again" servers 3
until_true 600 "three etcd members again" etcd_members 3
gone() { pve_get "cluster/resources?type=vm" | jq -e --argjson v "${vmid}" '[.data[] | select(.vmid == $v)] | length == 0' > /dev/null; }
until_true 600 "VM ${vmid} removed" gone
# shellcheck disable=SC2310 # its failure is the one reported.
workload -n default get configmap ha-quorum > /dev/null || fail "the write made with two etcd members is lost"

step "the control plane shrinks to one server"
kubectl -n "${ns}" patch cluster/ha --type=merge -p '{"spec":{"topology":{"controlPlane":{"replicas":1}}}}'
until_true 1800 "one server" servers 1
until_true 600 "one etcd member" etcd_members 1
until_true 300 "a write with one etcd member" written ha-single
# shellcheck disable=SC2310 # its failure is the one reported.
workload -n default get configmap ha-quorum > /dev/null || fail "the write made with two etcd members is lost"

step "delete the cluster"
kubectl -n "${ns}" delete cluster/ha --wait --timeout=30m
left=$(vms)
[ "${left}" = 0 ] || fail "VMs of the cluster are still there"
pools=$(pve_get pools)
printf '%s' "${pools}" | jq -e --arg p "/${ns}." '[.data[] | select(.poolid | contains($p))] | length == 0' > /dev/null ||
    fail "the cluster's pool is still there"
kubectl delete namespace "${ns}" --wait --timeout=5m

printf '\nPASS\n'
