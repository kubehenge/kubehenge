#!/bin/sh
# End-to-end test of a cluster on a Proxmox VE cluster of two nodes, against a running management
# cluster on the first: a k3s cluster kept to the second node is cloned there from a template on
# shared storage and comes up across the nodes' common network; kept to the first, every machine
# moves there; and with the second node down, Kubehenge leaves it out of the cluster's placement,
# so that the cluster, let onto every node again, keeps to the one online and grows there. With the
# second node back, nothing of the cluster is left once it is deleted.
#
#   test/e2e/two-nodes.sh
#
# KUBECONFIG points at the management cluster. KUBEHENGE_PROXMOX_URL and KUBEHENGE_PROXMOX_TOKEN
# reach Proxmox VE with rights to inspect everything; KUBEHENGE_PROXMOX_CA is the API's CA if the
# system does not trust it. Network, as for workload-cluster.sh: KUBEHENGE_E2E_BRIDGE,
# KUBEHENGE_E2E_PREFIX, KUBEHENGE_E2E_GATEWAY, KUBEHENGE_E2E_DNS; the cluster's node addresses, at
# least four, and virtual IP: KUBEHENGE_E2E_ADDRESSES, KUBEHENGE_E2E_ENDPOINT. KUBEHENGE_E2E_NODES
# names the two nodes, the first the management cluster's, and KUBEHENGE_E2E_SHARED_STORAGE a
# storage both share, holding the k3s template. KUBEHENGE_E2E_VERSION is the k3s version.
# KUBEHENGE_E2E_DOWN is a command that takes the second node out of the Proxmox VE cluster, the
# first keeping quorum on its own, and KUBEHENGE_E2E_UP one that brings it back: a cluster is
# deleted only with every node online, where node-local volumes of its may be left.
set -eu

: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}"
: "${KUBEHENGE_E2E_ADDRESSES:?}" "${KUBEHENGE_E2E_ENDPOINT:?}" "${KUBEHENGE_E2E_VERSION:?}"
: "${KUBEHENGE_E2E_NODES:?}" "${KUBEHENGE_E2E_SHARED_STORAGE:?}" "${KUBEHENGE_E2E_DOWN:?}" "${KUBEHENGE_E2E_UP:?}"

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

first=${KUBEHENGE_E2E_NODES%%,*}
second=${KUBEHENGE_E2E_NODES#*,}
[ -n "${first}" ] && [ -n "${second}" ] && [ "${first}" != "${second}" ] && [ "${second#*,}" = "${second}" ] ||
    fail "KUBEHENGE_E2E_NODES names two nodes, not ${KUBEHENGE_E2E_NODES}"

work=$(mktemp -d)
ns=e2e-nodes-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
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
pve_get() { curl -sS --fail ${curl_ca} -H "Authorization: PVEAPIToken=${KUBEHENGE_PROXMOX_TOKEN}" "${KUBEHENGE_PROXMOX_URL}/api2/json/$1"; }
json_list() { printf '%s' "$1" | jq -R 'split(",")'; }
workload() { KUBECONFIG="${work}/nodes" kubectl --request-timeout=20s "$@"; }

# vms_on: the nodes the cluster's VMs run on, the volume owner aside, sorted and without repeats.
vms_on() {
    pve_get "cluster/resources?type=vm" |
        jq -r --arg p "/${ns}." '[.data[] | select((.pool // "" | contains($p)) and .template != 1 and .status == "running") | .node] | unique | join(",")'
}
# all_on <node>: whether every machine of the cluster is on that node, up to date and ready.
all_on() {
    # shellcheck disable=SC2310 # polled; no answer is not every machine there.
    on=$(vms_on) || return 1
    [ "${on}" = "$1" ] || return 1
    kubectl -n "${ns}" get cluster/nodes -o json |
        jq -e '.status.controlPlane as $c | .status.workers as $w | $c.replicas == $c.desiredReplicas and $c.upToDateReplicas == $c.replicas and $c.readyReplicas == $c.replicas
            and $w.replicas == $w.desiredReplicas and $w.upToDateReplicas == $w.replicas and $w.readyReplicas == $w.replicas' > /dev/null
}
online_nodes() { kubectl -n "${ns}" get cluster/nodes -o jsonpath='{.metadata.annotations.kubehenge\.dev/online-nodes}'; }
online_are() {
    # shellcheck disable=SC2310 # polled; no answer is not the nodes looked for.
    now=$(online_nodes) || return 1
    [ "${now}" = "$1" ]
}
nodes_ready() {
    # shellcheck disable=SC2310 # polled; no answer is no node ready.
    n=$(workload get nodes -o json | jq '[.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length') || return 1
    [ "${n}" = "$1" ]
}
# allow <JSON list of nodes, or null>: keeps the cluster to the nodes, or lets it onto every node.
allow() {
    variables=$(kubectl -n "${ns}" get cluster/nodes -o json |
        jq -c --argjson nodes "$1" '.spec.topology.variables | map(select(.name != "allowedNodes")) + (if $nodes == null then [] else [{name: "allowedNodes", value: $nodes}] end)')
    kubectl -n "${ns}" patch cluster/nodes --type=json -p "[{\"op\": \"replace\", \"path\": \"/spec/topology/variables\", \"value\": ${variables}}]"
}

step "a k3s cluster kept to ${second}, cloned there from ${KUBEHENGE_E2E_SHARED_STORAGE}"
kubectl create namespace "${ns}"
addresses=$(json_list "${KUBEHENGE_E2E_ADDRESSES}")
dns_servers=$(json_list "${KUBEHENGE_E2E_DNS}")
kubectl apply -f - << EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: nodes
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
        value: ${KUBEHENGE_E2E_SHARED_STORAGE}
      - name: allowedNodes
        value: ["${second}"]
      # Proxmox VE HA fences a failed node through its watchdog, which the test's nodes lack.
      - name: proxmoxHA
        value: false
      - name: controlPlaneMachine
        value: {cores: ${KUBEHENGE_E2E_CONTROL_PLANE_CORES:-2}, memoryMiB: 4096}
      - name: workerMachine
        value: {cores: 2, memoryMiB: 1024}
EOF
kubectl -n "${ns}" wait --for=condition=Available cluster/nodes --timeout=40m
kubectl -n "${ns}" get secret nodes-kubeconfig -o jsonpath='{.data.value}' | base64 -d > "${work}/nodes"
until_true 600 "every machine on ${second}" all_on "${second}"
until_true 600 "both nodes ready" nodes_ready 2
until_true 120 "both Proxmox VE nodes online for the cluster" online_are "${first},${second}"

step "kept to ${first}, every machine moves there"
allow "[\"${first}\"]"
until_true 2400 "every machine on ${first}" all_on "${first}"
until_true 600 "both nodes ready" nodes_ready 2

step "with ${second} down, the cluster let onto every node keeps to ${first} and grows there"
sh -c "${KUBEHENGE_E2E_DOWN}"
until_true 300 "${second} left out of the cluster's placement" online_are "${first}"
allow null
kubectl -n "${ns}" patch cluster/nodes --type=json -p '[{"op": "replace", "path": "/spec/topology/workers/machineDeployments/0/replicas", "value": 2}]'
until_true 2400 "every machine, the new worker too, on ${first}" all_on "${first}"
until_true 600 "three nodes ready" nodes_ready 3

step "${second} back, and the cluster deleted"
sh -c "${KUBEHENGE_E2E_UP}"
until_true 600 "${second} back in the cluster's placement" online_are "${first},${second}"
kubectl -n "${ns}" delete cluster/nodes --wait --timeout=30m
left=$(pve_get "cluster/resources?type=vm" | jq --arg p "/${ns}." '[.data[] | select(.pool // "" | contains($p))] | length')
[ "${left}" = 0 ] || fail "VMs of the cluster are still there"
pools=$(pve_get pools)
printf '%s' "${pools}" | jq -e --arg p "/${ns}." '[.data[] | select(.poolid | contains($p))] | length == 0' > /dev/null ||
    fail "the cluster's pool is still there"
kubectl delete namespace "${ns}" --wait --timeout=5m

printf '\nPASS\n'
