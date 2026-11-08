#!/bin/sh
# End-to-end test of a highly available control plane on a Proxmox VE host, against a running
# management cluster: a k3s cluster's control plane grows from one server to three, and etcd's
# members with it. The server holding the cluster's virtual IP dies; the API stays reachable at that
# address from another server and takes writes, etcd keeping its quorum on the two left, until the
# control plane's health check has the dead server replaced. The control plane then shrinks back to
# one server, keeping what was written, and nothing of the cluster is left once it is deleted.
# Every node's NIC firewall is on throughout, through a security group that lets in SSH and the API
# alone: the servers join, elect and replace each other behind it, the runner reaches SSH and the
# API but not the kubelet or a NodePort, a pod reaches a pod on another node, and a change of the
# firewall's sources reaches the running VMs' IPSets without replacing one.
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
suffix=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
ns=e2e-ha-${suffix}
# The security group's name: 2 to 18 letters, digits, - and _.
group=e2eha-${suffix}
# What the run changed on Proxmox VE, which firewall_restore undoes: the datacenter firewall's
# state before the run, and whether the group exists.
firewall_before='' group_made=false
# shellcheck disable=SC2310 # best effort: a failed restore must not hide the run's own status.
firewall_restore() {
    if [ "${group_made}" = true ]; then
        # The VMs hold rules that name the group until they are gone, so a run that fails leaves it.
        pve -X DELETE "${KUBEHENGE_PROXMOX_URL}/api2/json/cluster/firewall/groups/${group}" > /dev/null 2>&1 || true
        group_made=false
    fi
    if [ -n "${firewall_before}" ]; then
        pve -X PUT -d "enable=${firewall_before}" "${KUBEHENGE_PROXMOX_URL}/api2/json/cluster/firewall/options" > /dev/null 2>&1 || true
        firewall_before=
    fi
}
cleanup() {
    status=$?
    if [ "${status}" -ne 0 ]; then
        printf '\nleaving the cluster in %s for inspection; delete it with: kubectl delete namespace %s\n' "${ns}" "${ns}"
    fi
    firewall_restore
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

# cluster_vms: the node and ID of each VM the cluster's pool holds, one per line.
cluster_vms() {
    pve_get "cluster/resources?type=vm" | jq -r --arg p "/${ns}." '.data[] | select(.pool // "" | contains($p)) | "\(.node) \(.vmid)"'
}
# ipset_entries <node> <vmid>: the entries of the VM's kubehenge IPSet, sorted, one per line. A VM
# without the IPSet has none.
ipset_entries() { pve_get "nodes/$1/qemu/$2/firewall/ipset/kubehenge" | jq -r '.data[].cidr' | sort; }
# The three checks below are polled, so a failing step counts as not yet.
# ipsets_hold <present|absent> <address>: whether every VM's IPSet holds the address, or none does.
# shellcheck disable=SC2310,SC2312 # polled.
ipsets_hold() {
    vm_list=$(cluster_vms)
    [ -n "${vm_list}" ] || return 1
    echo "${vm_list}" | while read -r vm_node vm_id; do
        if ipset_entries "${vm_node}" "${vm_id}" | grep -qxF "$2"; then held=present; else held=absent; fi
        [ "${held}" = "$1" ] || exit 1
    done
}
# ipsets_agree: whether every VM's IPSet holds the same entries, which one cluster's VMs share.
# shellcheck disable=SC2310,SC2312 # polled.
ipsets_agree() {
    vm_list=$(cluster_vms)
    seen=$(echo "${vm_list}" | while read -r vm_node vm_id; do
        ipset_entries "${vm_node}" "${vm_id}" | tr '\n' ' '
        echo
    done | sort -u)
    [ -n "${vm_list}" ] && [ "$(echo "${seen}" | wc -l)" -eq 1 ]
}
# firewalled_nics: whether every NIC of every VM has firewall=1.
# shellcheck disable=SC2310,SC2312 # polled.
firewalled_nics() {
    vm_list=$(cluster_vms)
    [ -n "${vm_list}" ] || return 1
    echo "${vm_list}" | while read -r vm_node vm_id; do
        pve_get "nodes/${vm_node}/qemu/${vm_id}/config" |
            jq -e '[.data | to_entries[] | select(.key | test("^net[0-9]+$")) | .value] | length > 0 and all(contains("firewall=1"))' > /dev/null || exit 1
    done
}
# port_state <address> <port>: dropped when the connection times out, as a firewall's drop does,
# refused when nothing listens, and open when something answers.
port_state() {
    rc=0
    curl -s --connect-timeout 4 -o /dev/null "http://$1:$2/" || rc=$?
    case ${rc} in
        28) echo dropped ;;
        7) echo refused ;;
        *) echo open ;;
    esac
}
# every_node_is <state> <port>: whether the runner finds the port in that state on every node.
every_node_is() {
    for address in ${node_addresses}; do
        seen="${address}:$2 is $(port_state "${address}" "$2")"
        case ${seen} in
            *" is $1") ;;
            *) return 1 ;;
        esac
    done
}
# fixture_pod <name> <node> <args...>: an agnhost pod on the node, at Pod Security's restricted level.
fixture_pod() {
    fixture_name=$1 fixture_node=$2
    shift 2
    jq -n --arg name "${fixture_name}" --arg image "${agnhost}" --arg node "${fixture_node}" '$ARGS.positional as $args | {
        apiVersion: "v1", kind: "Pod", metadata: {name: $name, labels: {app: $name}},
        spec: {nodeName: $node,
            securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: "RuntimeDefault"}},
            containers: [{name: $name, image: $image, args: $args,
                resources: {requests: {cpu: "10m", memory: "16Mi"}, limits: {memory: "128Mi"}},
                securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}]}}' --args -- "$@" |
        workload -n e2e-firewall apply -f -
}

step "a security group that lets in SSH and the API, and the datacenter firewall"
# The API's port is in the group because the runner, like any administrator's machine, is no source
# of the cluster's own, and the steps below reach the cluster through its virtual IP.
firewall_before=$(pve_get cluster/firewall/options | jq -r '.data.enable // 0')
pve -X PUT -d enable=1 "${KUBEHENGE_PROXMOX_URL}/api2/json/cluster/firewall/options" > /dev/null
pve -X POST -d "group=${group}" -d "comment=Kubehenge end-to-end test" "${KUBEHENGE_PROXMOX_URL}/api2/json/cluster/firewall/groups" > /dev/null
group_made=true
for port in 22 6443; do
    pve -X POST -d type=in -d action=ACCEPT -d proto=tcp -d "dport=${port}" -d enable=1 \
        "${KUBEHENGE_PROXMOX_URL}/api2/json/cluster/firewall/groups/${group}" > /dev/null
done

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
      - name: securityGroups
        value: [${group}]
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

step "the firewall lets in SSH and the API, keeps out the kubelet and NodePorts, and passes the cluster's own traffic"
until_true 120 "every NIC with firewall=1" firewalled_nics
node_addresses=$(workload get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')
node_count=$(echo "${node_addresses}" | wc -l)
[ "${node_count}" -eq 4 ] || fail "expected four nodes' addresses, have: ${node_addresses}"
until_true 120 "SSH answers on every node" every_node_is open 22
until_true 120 "the kubelet's port is dropped on every node" every_node_is dropped 10250
workload create namespace e2e-firewall
node_names=$(workload get nodes -o jsonpath='{.items[*].metadata.name}')
server_node=${node_names%% *}
rest=${node_names#* }
client_node=${rest%% *}
fixture_pod server "${server_node}" netexec --http-port=8080
fixture_pod client "${client_node}" pause
workload -n e2e-firewall wait --for=condition=Ready pod/server pod/client --timeout=10m
workload -n e2e-firewall create service nodeport server --tcp=8080:8080
node_port=$(workload -n e2e-firewall get service server -o jsonpath='{.spec.ports[0].nodePort}')
until_true 120 "the NodePort ${node_port} is dropped on every node" every_node_is dropped "${node_port}"
server_ip=$(workload -n e2e-firewall get pod server -o jsonpath='{.status.podIP}')
server_node_address=$(workload get node "${server_node}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
# The exec itself runs through the kubelet's port, which the firewall keeps from the runner but
# gives the API servers.
reaches() { workload -n e2e-firewall exec client -- /agnhost connect "$1" --timeout=3s > /dev/null 2>&1; }
until_true 120 "a pod reaches a pod on another node" reaches "${server_ip}:8080"
until_true 120 "a pod reaches the NodePort the runner cannot" reaches "${server_node_address}:${node_port}"
workload delete namespace e2e-firewall --wait --timeout=5m

step "a changed source reaches the running VMs' IPSets, replacing none"
vms_before=$(cluster_vms | sort)
source_address=192.0.2.40
until_true 60 "no VM's IPSet holds ${source_address}" ipsets_hold absent "${source_address}"
until_true 120 "every VM's IPSet holds the nodes' addresses, one set" ipsets_agree
# The appliance's record, which the controller adds to every firewalled cluster's sources. No
# appliance runs in this lane, so the test writes it.
addresses_before=$(kubectl -n kubehenge-system get configmap kubehenge-appliance-addresses -o jsonpath='{.data.addresses}')
set_appliance_addresses() {
    patch=$(jq -nc --arg a "$1" '{data: {addresses: $a}}')
    kubectl -n kubehenge-system patch configmap kubehenge-appliance-addresses --type=merge -p "${patch}"
}
set_appliance_addresses "${source_address}"
until_true 180 "every VM's IPSet holds ${source_address}" ipsets_hold present "${source_address}"
set_appliance_addresses "${addresses_before}"
until_true 180 "no VM's IPSet holds ${source_address}" ipsets_hold absent "${source_address}"
vms_after=$(cluster_vms | sort)
[ "${vms_after}" = "${vms_before}" ] || fail "a source's change replaced a VM"

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
# By its name: Proxmox VE hands the freed VM ID to the next clone, the replacement's often.
gone() { pve_get "cluster/resources?type=vm" | jq -e --arg n "${infra}" '[.data[] | select(.name == $n)] | length == 0' > /dev/null; }
until_true 600 "VM ${vmid} (${infra}) removed" gone
# The replacement got its firewall before it first started, and joined the cluster behind it.
until_true 300 "every NIC with firewall=1 again" firewalled_nics
until_true 300 "every VM's IPSet the same again" ipsets_agree
# shellcheck disable=SC2310 # its failure is the one reported.
workload -n default get configmap ha-quorum > /dev/null || fail "the write made with two etcd members is lost"

step "the control plane shrinks to one server"
kubectl -n "${ns}" patch cluster/ha --type=merge -p '{"spec":{"topology":{"controlPlane":{"replicas":1}}}}'
until_true 1800 "one server" servers 1
until_true 600 "one etcd member" etcd_members 1
until_true 300 "a write with one etcd member" written ha-single
# shellcheck disable=SC2310 # its failure is the one reported.
workload -n default get configmap ha-quorum > /dev/null || fail "the write made with two etcd members is lost"

step "delete the cluster, its ProxmoxCluster first, as a namespace delete can"
proxmox_cluster=$(kubectl -n "${ns}" get cluster/ha -o jsonpath='{.spec.infrastructureRef.name}')
kubectl -n "${ns}" delete "proxmoxclusters.infrastructure.cluster.x-k8s.io/${proxmox_cluster}" --wait=false
marked() {
    marked_at=$(kubectl -n "${ns}" get "proxmoxclusters.infrastructure.cluster.x-k8s.io/${proxmox_cluster}" -o jsonpath='{.metadata.deletionTimestamp}') || return 1
    [ -n "${marked_at}" ]
}
until_true 60 "the ProxmoxCluster marked for deletion" marked
# capmox has to reconcile that deletion while the Cluster still stands. Unpatched, it never looks
# again once the Cluster goes, and the delete below runs out of time.
sleep 15
kubectl -n "${ns}" delete cluster/ha --wait --timeout=30m
left=$(vms)
[ "${left}" = 0 ] || fail "VMs of the cluster are still there"
pools=$(pve_get pools)
printf '%s' "${pools}" | jq -e --arg p "/${ns}." '[.data[] | select(.poolid | contains($p))] | length == 0' > /dev/null ||
    fail "the cluster's pool is still there"
firewall_restore
group_gone() { pve_get cluster/firewall/groups | jq -e --arg g "${group}" '[.data[] | select(.group == $g)] | length == 0' > /dev/null; }
until_true 60 "the security group deleted" group_gone
kubectl delete namespace "${ns}" --wait --timeout=5m

printf '\nPASS\n'
