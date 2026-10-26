#!/bin/sh
# End-to-end test of Cilium Cluster Mesh on a Proxmox VE host, against a running management cluster:
# two one-node k3s clusters that run Cilium, each listing the other, are linked through their
# clustermesh-apiservers on NodePort 32379 of their virtual IPs; a pod of one reaches a pod of the
# other by address and through a global service; once one stops listing the other the link goes,
# and once one leaves the mesh its Cluster Mesh objects go with it; nothing of either is left
# afterwards.
#
#   test/e2e/cluster-mesh.sh
#
# KUBECONFIG points at the management cluster. KUBEHENGE_PROXMOX_URL and KUBEHENGE_PROXMOX_TOKEN
# reach Proxmox VE with rights to inspect everything; KUBEHENGE_PROXMOX_CA is the API's CA if the
# system does not trust it. Network and storage, as for workload-cluster.sh: KUBEHENGE_E2E_BRIDGE,
# KUBEHENGE_E2E_PREFIX, KUBEHENGE_E2E_GATEWAY, KUBEHENGE_E2E_DNS, KUBEHENGE_E2E_STORAGE. Each cluster
# has its own addresses and virtual IP: KUBEHENGE_E2E_EAST_ADDRESSES and _EAST_ENDPOINT,
# KUBEHENGE_E2E_WEST_ADDRESSES and _WEST_ENDPOINT. KUBEHENGE_E2E_VERSION is the k3s version of both.
# KUBEHENGE_E2E_CORES and _MEMORY (MiB) size each node, by default the least of a k3s control plane
# without workers.
set -eu

: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_STORAGE:?}"
: "${KUBEHENGE_E2E_EAST_ADDRESSES:?}" "${KUBEHENGE_E2E_EAST_ENDPOINT:?}"
: "${KUBEHENGE_E2E_WEST_ADDRESSES:?}" "${KUBEHENGE_E2E_WEST_ENDPOINT:?}"
: "${KUBEHENGE_E2E_VERSION:?}"

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

work=$(mktemp -d)
ns=e2e-mesh-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
cleanup() {
    status=$?
    if [ "${status}" -ne 0 ]; then
        printf '\nleaving the clusters in %s for inspection; delete them with: kubectl delete namespace %s\n' "${ns}" "${ns}"
    fi
    rm -rf "${work}"
}
trap cleanup EXIT

curl_ca=
[ -n "${KUBEHENGE_PROXMOX_CA:-}" ] && curl_ca="--cacert ${KUBEHENGE_PROXMOX_CA}"
# shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
pve_get() { curl -sS --fail ${curl_ca} -H "Authorization: PVEAPIToken=${KUBEHENGE_PROXMOX_TOKEN}" "${KUBEHENGE_PROXMOX_URL}/api2/json/$1"; }
json_list() { printf '%s' "$1" | jq -R 'split(",")'; }
east() { KUBECONFIG="${work}/east" kubectl "$@"; }
west() { KUBECONFIG="${work}/west" kubectl "$@"; }

# cluster <name> <addresses> <endpoint> <pod range> <peer>: a one-node k3s cluster that runs Cilium
# in a Cluster Mesh with the peer. Applied with kubectl, which gives a cluster no range of its own as
# the interface does, so each is given one apart from the other's.
cluster() {
    addresses=$(json_list "$2")
    dns_servers=$(json_list "${KUBEHENGE_E2E_DNS}")
    kubectl apply -f - << EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: $1
  namespace: ${ns}
spec:
  clusterNetwork:
    pods:
      cidrBlocks: [$4]
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
        value: $3
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
        value: {cores: ${KUBEHENGE_E2E_CORES:-3}, memoryMiB: ${KUBEHENGE_E2E_MEMORY:-5120}}
      - name: cni
        value: cilium
      - name: podSecurity
        value: {admission: restricted}
      - name: clusterMesh
        value: {enabled: true, peers: [$5]}
EOF
}
kubeconfig() { kubectl -n "${ns}" get secret "$1-kubeconfig" -o jsonpath='{.data.value}' | base64 -d > "${work}/$1"; }
# set_variable <cluster> <name> <JSON value>: sets one of a Cluster's topology variables.
set_variable() {
    vars=$(kubectl -n "${ns}" get "cluster/$1" -o json |
        jq -c --arg n "$2" --argjson v "$3" '[.spec.topology.variables[] | select(.name != $n)] + [{name: $n, value: $v}]')
    kubectl -n "${ns}" patch "cluster/$1" --type=merge -p "{\"spec\":{\"topology\":{\"variables\":${vars}}}}"
}
# peer_ready <kubeconfig> <peer>: whether the cluster's Cilium agent is connected to the peer.
peer_ready() {
    KUBECONFIG="${work}/$1" kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status -o json 2> /dev/null |
        jq -e --arg p "$2" '[.["cluster-mesh"].clusters[]? | select(.name == $p and .ready)] | length == 1' > /dev/null
}
# reads_peer <kubeconfig> <peer>: whether the cluster's KVStoreMesh is given the peer to read.
reads_peer() {
    KUBECONFIG="${work}/$1" kubectl -n kube-system get secret cilium-kvstoremesh -o json |
        jq -e --arg p "$2" '.data // {} | has($p)' > /dev/null
}
# The pods: restricted, as the clusters enforce.
agnhost_pod() { # <kubeconfig> <name> <args...>
    agnhost_overrides=$(printf '{"apiVersion":"v1","spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"%s","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' "$2")
    agnhost_kubeconfig=$1 agnhost_name=$2
    shift 2
    KUBECONFIG="${work}/${agnhost_kubeconfig}" kubectl -n e2e-mesh run "${agnhost_name}" --image="${agnhost}" --labels="app=${agnhost_name}" \
        --override-type=strategic --overrides="${agnhost_overrides}" -- "$@"
}
# A global service: Cilium merges the endpoints of the services of its name and namespace in every
# linked cluster, so west's, without pods of its own, leads to east's server.
global_service() {
    KUBECONFIG="${work}/$1" kubectl -n e2e-mesh apply -f - << 'EOF'
apiVersion: v1
kind: Service
metadata:
  name: echo
  annotations:
    service.cilium.io/global: "true"
spec:
  selector:
    app: server
  ports:
    - port: 8080
EOF
}

step "two k3s clusters that run Cilium, each listing the other"
kubectl create namespace "${ns}"
cluster east "${KUBEHENGE_E2E_EAST_ADDRESSES}" "${KUBEHENGE_E2E_EAST_ENDPOINT}" 10.201.0.0/16 west
cluster west "${KUBEHENGE_E2E_WEST_ADDRESSES}" "${KUBEHENGE_E2E_WEST_ENDPOINT}" 10.202.0.0/16 east
kubectl -n "${ns}" wait --for=condition=Available cluster/east cluster/west --timeout=40m
kubeconfig east
kubeconfig west
east_id=$(kubectl -n "${ns}" get cluster/east -o jsonpath='{.metadata.annotations.kubehenge\.dev/cilium-cluster-id}')
west_id=$(kubectl -n "${ns}" get cluster/west -o jsonpath='{.metadata.annotations.kubehenge\.dev/cilium-cluster-id}')
[ -n "${east_id}" ] && [ -n "${west_id}" ] && [ "${east_id}" != "${west_id}" ] ||
    fail "the clusters have no Cilium cluster ids of their own: east ${east_id}, west ${west_id}"
for c in east west; do
    id=$(KUBECONFIG="${work}/${c}" kubectl -n kube-system get configmap cilium-config -o jsonpath='{.data.cluster-name} {.data.cluster-id}')
    want=$(kubectl -n "${ns}" get "cluster/${c}" -o jsonpath='{.metadata.name} {.metadata.annotations.kubehenge\.dev/cilium-cluster-id}')
    [ "${id}" = "${want}" ] || fail "${c}'s Cilium knows itself as ${id}, not ${want}"
done

step "linked through the clustermesh-apiservers on the virtual IPs"
until_true 900 "east's clustermesh-apiserver" east -n kube-system wait --for=condition=Available deployment/clustermesh-apiserver --timeout=10s
until_true 900 "west's clustermesh-apiserver" west -n kube-system wait --for=condition=Available deployment/clustermesh-apiserver --timeout=10s
until_true 600 "east reads west" reads_peer east west
until_true 600 "west reads east" reads_peer west east
until_true 600 "east connected to west" peer_ready east west
until_true 600 "west connected to east" peer_ready west east

step "a pod of one reaches a pod of the other, by address and through a global service"
new_namespace east e2e-mesh
new_namespace west e2e-mesh
agnhost_pod east server netexec --http-port=8080
agnhost_pod west client pause
east -n e2e-mesh wait --for=condition=Ready pod/server --timeout=5m
west -n e2e-mesh wait --for=condition=Ready pod/client --timeout=5m
server_ip=$(east -n e2e-mesh get pod server -o jsonpath='{.status.podIP}')
case ${server_ip} in
    10.201.*) ;;
    *) fail "east's server has the address ${server_ip}, outside east's pod range" ;;
esac
# shellcheck disable=SC2310 # polled with set -e off, so a failed fetch is only a retry.
fetches() { fetched=$(west -n e2e-mesh exec client -- curl -sS --max-time 5 "http://$1:8080/hostname" 2>&1) && [ "${fetched}" = server ]; }
until_true 120 "west's client reaches east's server at its address" fetches "${server_ip}"
global_service east
global_service west
until_true 120 "west's client reaches east's server through the global service" fetches echo.e2e-mesh.svc.cluster.local
echo "memory with Cluster Mesh:"
pod_memory "${work}/east"
for node in $(east get nodes -o jsonpath='{.items[*].metadata.name}'); do
    east get --raw "/api/v1/nodes/${node}/proxy/stats/summary" |
        jq -r '.pods[] | select(.podRef.name | startswith("clustermesh-apiserver")) | "  clustermesh-apiserver: \(.memory.workingSetBytes / 1048576 | floor) MiB"'
done

step "the link goes once west stops listing east"
set_variable west clusterMesh '{"enabled":true,"peers":[]}'
# shellcheck disable=SC2310 # the failure is the answer looked for.
forgot_peer() { ! reads_peer "$@"; }
until_true 300 "east no longer reads west" forgot_peer east west
until_true 300 "west no longer reads east" forgot_peer west east
not_linked() {
    kubectl -n "${ns}" get events --field-selector involvedObject.name=east,reason=ClusterMeshNotLinked -o json |
        jq -e '[.items[] | select(.message | test("west: it does not list this cluster"))] | length > 0' > /dev/null
}
until_true 300 "east says why it is not linked" not_linked
# shellcheck disable=SC2310 # the failure is the answer looked for.
unreachable() { ! fetches echo.e2e-mesh.svc.cluster.local; }
until_true 300 "west's client no longer reaches east's server" unreachable

step "east leaves the mesh, which takes its Cluster Mesh objects out"
set_variable east clusterMesh '{"enabled":false}'
until_true 900 "east's clustermesh-apiserver removed" absent east -n kube-system get deployment/clustermesh-apiserver
until_true 300 "east's peers removed" absent east -n kube-system get secret/cilium-kvstoremesh
until_true 300 "east's certificates removed" absent east -n kube-system get secret/clustermesh-apiserver-remote-cert
unmarked() {
    mark=$(kubectl -n "${ns}" get cluster/east -o jsonpath='{.metadata.annotations.kubehenge\.dev/cluster-mesh}') && [ -z "${mark}" ]
}
until_true 300 "east unmarked" unmarked
kubectl -n "${ns}" get secret kubehenge-cluster-mesh-ca -o name > /dev/null || fail "the namespace's Cluster Mesh CA is gone while west still uses it"

step "delete both clusters"
kubectl -n "${ns}" delete cluster/east cluster/west --wait --timeout=30m
vms=$(pve_get "cluster/resources?type=vm")
printf '%s' "${vms}" | jq -e --arg p "/${ns}." '[.data[] | select(.pool // "" | contains($p))] | length == 0' > /dev/null ||
    fail "VMs of the clusters are still there"
pools=$(pve_get pools)
printf '%s' "${pools}" | jq -e --arg p "/${ns}." '[.data[] | select(.poolid | contains($p))] | length == 0' > /dev/null ||
    fail "pools of the clusters are still there"
kubectl delete namespace "${ns}" --wait --timeout=5m

printf '\nPASS\n'
