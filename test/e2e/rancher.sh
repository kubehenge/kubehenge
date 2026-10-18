#!/bin/sh
# End-to-end test of the Rancher add-on on a Proxmox VE host, against a running management cluster:
# a one-node RKE2 cluster installs Rancher, a one-node k3s cluster registers with it on its own,
# leaves it (Rancher cleans up inside it) and comes back, the Rancher cluster is held at the
# Kubernetes versions Rancher runs on, the k3s cluster leaves Rancher before it is deleted, and
# nothing of either is left afterwards.
#
#   test/e2e/rancher.sh
#
# KUBECONFIG points at the management cluster. KUBEHENGE_PROXMOX_URL and KUBEHENGE_PROXMOX_TOKEN
# reach Proxmox VE with rights to inspect everything; KUBEHENGE_PROXMOX_CA is the API's CA if the
# system does not trust it. Network and storage, as for workload-cluster.sh: KUBEHENGE_E2E_BRIDGE,
# KUBEHENGE_E2E_PREFIX, KUBEHENGE_E2E_GATEWAY, KUBEHENGE_E2E_DNS, KUBEHENGE_E2E_STORAGE. Each cluster
# has its own addresses and virtual IP: KUBEHENGE_E2E_RANCHER_ADDRESSES and _RANCHER_ENDPOINT,
# KUBEHENGE_E2E_DOWNSTREAM_ADDRESSES and _DOWNSTREAM_ENDPOINT. KUBEHENGE_E2E_RANCHER_HOSTNAME
# resolves to the Rancher cluster's virtual IP, where its ingress controller answers; by default
# <endpoint>.sslip.io. KUBEHENGE_E2E_RANCHER_VERSION (RKE2) and KUBEHENGE_E2E_DOWNSTREAM_VERSION (k3s)
# lie within the Kubernetes versions the installed Rancher runs on; KUBEHENGE_E2E_BLOCKED_VERSION
# lies beyond them. KUBEHENGE_E2E_RANCHER_MEMORY and _DOWNSTREAM_MEMORY (MiB) size the nodes, and
# KUBEHENGE_E2E_RANCHER_CORES Rancher's, at least the 4 Kubehenge requires.
set -eu

: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_STORAGE:?}"
: "${KUBEHENGE_E2E_RANCHER_ADDRESSES:?}" "${KUBEHENGE_E2E_RANCHER_ENDPOINT:?}"
: "${KUBEHENGE_E2E_DOWNSTREAM_ADDRESSES:?}" "${KUBEHENGE_E2E_DOWNSTREAM_ENDPOINT:?}"
: "${KUBEHENGE_E2E_RANCHER_VERSION:?}" "${KUBEHENGE_E2E_DOWNSTREAM_VERSION:?}" "${KUBEHENGE_E2E_BLOCKED_VERSION:?}"
hostname=${KUBEHENGE_E2E_RANCHER_HOSTNAME:-${KUBEHENGE_E2E_RANCHER_ENDPOINT}.sslip.io}

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

work=$(mktemp -d)
ns=e2e-rancher-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
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
rancher() { KUBECONFIG="${work}/rancher" kubectl "$@"; }
downstream() { KUBECONFIG="${work}/downstream" kubectl "$@"; }

# cluster <name> <class> <version> <addresses> <endpoint> <control-plane machine> <rancher variable> <pod security level>
cluster() {
    addresses=$(json_list "$4")
    dns_servers=$(json_list "${KUBEHENGE_E2E_DNS}")
    kubectl apply -f - << EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: $1
  namespace: ${ns}
spec:
  topology:
    classRef:
      name: $2
      namespace: kubehenge-system
    version: $3
    controlPlane:
      replicas: 1
    workers:
      machineDeployments:
        - class: worker
          name: md-0
          replicas: 0
    variables:
      - name: controlPlaneEndpoint
        value: $5
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
        value: $6
      - name: rancher
        value: $7
      - name: podSecurity
        value: {admission: $8}
EOF
}
kubeconfig() { kubectl -n "${ns}" get secret "$1-kubeconfig" -o jsonpath='{.data.value}' | base64 -d > "${work}/$2"; }
# set_variable <cluster> <name> <JSON value>: sets one of a Cluster's topology variables.
set_variable() {
    vars=$(kubectl -n "${ns}" get "cluster/$1" -o json |
        jq -c --arg n "$2" --argjson v "$3" '[.spec.topology.variables[] | select(.name != $n)] + [{name: $n, value: $v}]')
    kubectl -n "${ns}" patch "cluster/$1" --type=merge -p "{\"spec\":{\"topology\":{\"variables\":${vars}}}}"
}
# Kubehenge names a cluster in Rancher <namespace>-<name>-<first 8 hex of sha256(namespace/name)>.
in_rancher="${ns}-downstream-$(printf '%s' "${ns}/downstream" | sha256sum | cut -c1-8)"
registered() { rancher -n fleet-default get clusters.provisioning.cattle.io "${in_rancher}" -o jsonpath='{.status.ready}' 2> /dev/null | grep -q true; }
gone_from_rancher() { absent rancher -n fleet-default get clusters.provisioning.cattle.io "${in_rancher}"; }

step "Rancher's cluster, RKE2 ${KUBEHENGE_E2E_RANCHER_VERSION}, Rancher at ${hostname}"
kubectl create namespace "${ns}"
cluster rancher kubehenge-rke2 "${KUBEHENGE_E2E_RANCHER_VERSION}" "${KUBEHENGE_E2E_RANCHER_ADDRESSES}" "${KUBEHENGE_E2E_RANCHER_ENDPOINT}" \
    "{cores: ${KUBEHENGE_E2E_RANCHER_CORES:-4}, memoryMiB: ${KUBEHENGE_E2E_RANCHER_MEMORY:-8192}}" "{install: true, hostname: ${hostname}}" restricted
kubectl -n "${ns}" wait --for=condition=Available cluster/rancher --timeout=40m
kubeconfig rancher rancher
until_true 1200 "Rancher installed" sh -c "KUBECONFIG=${work}/rancher kubectl -n cattle-system get deployment/rancher >/dev/null 2>&1"
# Available, not `rollout status`: a first start slower than the Deployment's progress deadline only
# marks it, and Rancher still comes up.
rancher -n cattle-system wait --for=condition=Available deployment/rancher --timeout=30m
# Rancher's own CA, which every agent pins.
until_true 600 "Rancher's CA" sh -c "KUBECONFIG=${work}/rancher kubectl get settings.management.cattle.io cacerts -o jsonpath='{.value}' | grep -q 'BEGIN CERTIFICATE'"
rancher get settings.management.cattle.io cacerts -o jsonpath='{.value}' > "${work}/rancher-ca.pem"
until_true 600 "Rancher answering at ${hostname}" sh -c "curl -sS --fail --cacert ${work}/rancher-ca.pem https://${hostname}/ping | grep -q pong"
server_url=$(rancher get settings.management.cattle.io server-url -o jsonpath='{.value}')
[ "${server_url}" = "https://${hostname}" ] || fail "Rancher's server URL is not https://${hostname}"
# shellcheck disable=SC2310 # rancher wraps one kubectl call, whose status is the one tested.
rancher -n cattle-system get secret bootstrap-secret > /dev/null || fail "Rancher generated no first admin password"

step "a k3s cluster registers with Rancher on its own"
# Rancher's agents and controllers run in the namespaces Kubehenge exempts, but the job that cleans
# up after them runs in default and is refused at restricted until a Rancher release carries
# rancher/rancher#58081's fix (docs/troubleshooting.md#rancher).
cluster downstream kubehenge-k3s "${KUBEHENGE_E2E_DOWNSTREAM_VERSION}" "${KUBEHENGE_E2E_DOWNSTREAM_ADDRESSES}" "${KUBEHENGE_E2E_DOWNSTREAM_ENDPOINT}" \
    "{memoryMiB: ${KUBEHENGE_E2E_DOWNSTREAM_MEMORY:-4096}}" "{}" baseline
kubectl -n "${ns}" wait --for=condition=Available cluster/downstream --timeout=40m
kubeconfig downstream downstream
until_true 1200 "registered and connected" registered
downstream -n cattle-system rollout status deployment/cattle-cluster-agent --timeout=10m
agent=$(downstream -n cattle-system get deployment/cattle-cluster-agent -o json)
printf '%s' "${agent}" |
    jq -e '.spec.template.spec.containers[0].env | any(.name == "STRICT_VERIFY" and .value == "true") and any(.name == "CATTLE_CA_CHECKSUM" and (.value | length) == 64)' > /dev/null ||
    fail "the agent does not pin Rancher's CA"

step "the k3s cluster leaves Rancher, which cleans up inside it, and comes back"
set_variable downstream rancher '{"register":false}'
until_true 900 "removed from Rancher" gone_from_rancher
until_true 900 "Rancher's agent removed from the cluster" absent downstream get namespace cattle-system
set_variable downstream rancher '{"register":true}'
until_true 1200 "registered again" registered

step "Rancher's cluster is held at the Kubernetes versions Rancher runs on"
kubectl -n "${ns}" patch cluster/rancher --type=merge -p "{\"spec\":{\"topology\":{\"version\":\"${KUBEHENGE_E2E_BLOCKED_VERSION}\"}}}"
until_true 300 "upgrade held back" sh -c "kubectl -n ${ns} get cluster/rancher -o json | jq -e '[.status.conditions[]? | select(.message // \"\" | test(\"Rancher release\"))] | length > 0' >/dev/null"
sleep 60
# RKE2 and k3s kubelets report the distribution's full version, +rke2r1 or +k3s1 included.
kubelet=$(rancher get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')
[ "${kubelet}" = "${KUBEHENGE_E2E_RANCHER_VERSION}" ] || fail "Rancher's cluster was upgraded past the versions Rancher runs on"

step "deleting the k3s cluster removes it from Rancher first"
kubectl -n "${ns}" delete cluster/downstream --wait=false
until_true 900 "removed from Rancher" gone_from_rancher
kubectl -n "${ns}" wait --for=delete cluster/downstream --timeout=30m

step "delete Rancher's cluster"
kubectl -n "${ns}" delete cluster/rancher --wait --timeout=30m
vms=$(pve_get "cluster/resources?type=vm")
printf '%s' "${vms}" | jq -e --arg p "/${ns}." '[.data[] | select(.pool // "" | contains($p))] | length == 0' > /dev/null ||
    fail "VMs of the clusters are still there"
pools=$(pve_get pools)
printf '%s' "${pools}" | jq -e --arg p "/${ns}." '[.data[] | select(.poolid | contains($p))] | length == 0' > /dev/null ||
    fail "pools of the clusters are still there"
kubectl delete namespace "${ns}" --wait --timeout=5m

printf '\nPASS\n'
