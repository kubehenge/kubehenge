#!/bin/sh
# End-to-end test of one Kubehenge ClusterClass on a Proxmox VE host, against a running management
# cluster: create a cluster trusting an OpenID issuer, hardened to the CIS benchmark and enforcing
# Pod Security's restricted level, check sign-in through it, its nodes against the benchmark, its
# cloud controller manager, ingress controller, Gateway API, load balancer and persistent volumes,
# what the health page's read-only identities may see in it, switch Headlamp, Kyverno, Istio (a
# namespace's traffic over mutual TLS), monitoring (every component scraped with its server
# verified) and logs (a pod's lines, and with the node collector the journal and the audit log, read
# in Loki through Grafana) on and off, back a namespace with a volume and a Secret up with Velero to
# an S3 store the test serves, delete it and restore it, check the store holds the backup
# encrypted, switch backups and the load balancer off, scale its workers up and down, upgrade it by
# one minor version (for RKE2 and k3s with the first
# new server's join refused at first), replace a node that dies, delete it, and check that nothing
# of it is left on Proxmox VE.
#
#   test/e2e/workload-cluster.sh <ClusterClass> <version> <upgrade version>
#
# KUBECONFIG points at the management cluster. KUBEHENGE_PROXMOX_URL and KUBEHENGE_PROXMOX_TOKEN
# (user@realm!name=secret) reach Proxmox VE with rights to inspect everything, to destroy one of
# the cluster's VMs, standing in for a node that dies, and to run commands in its VMs through the
# guest agent (VM.GuestAgent.Unrestricted); KUBEHENGE_PROXMOX_CA is the API's CA if the
# system does not trust it. The cluster's network and storage:
#   KUBEHENGE_E2E_BRIDGE, KUBEHENGE_E2E_ADDRESSES (comma-separated ranges or networks),
#   KUBEHENGE_E2E_PREFIX, KUBEHENGE_E2E_GATEWAY, KUBEHENGE_E2E_DNS (comma-separated),
#   KUBEHENGE_E2E_ENDPOINT (the control plane's virtual IP), KUBEHENGE_E2E_STORAGE,
#   KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES (comma-separated, unused, on the nodes' network and
#   reached from the machine running this test). KUBEHENGE_E2E_LOAD_BALANCER is the load balancer,
#   metallb (default) or kube-vip.
# KUBEHENGE_E2E_CONTROL_PLANE_MEMORY and KUBEHENGE_E2E_WORKER_MEMORY (MiB) and
# KUBEHENGE_E2E_CONTROL_PLANE_CORES size the nodes; the worker's memory is by default the least
# the class lets it have with the add-ons. KUBEHENGE_E2E_RESTARTS=report prints restarts while every
# add-on is on instead of failing, for a host whose KVM lacks pause-loop exiting: there guests that
# outnumber its CPUs starve each other, whatever their sizes.
# KUBEHENGE_E2E_CNI chooses the CNI (required by kubeadm). KUBEHENGE_E2E_PODS and
# KUBEHENGE_E2E_SERVICES set the cluster's pod and service ranges, which must not overlap the nodes'
# network; unset, the distribution's defaults apply.
# KUBEHENGE_E2E_OIDC_ADDRESS is an IPv4 address of the machine running this test that the cluster's
# nodes reach; the test serves an OpenID issuer there, on KUBEHENGE_E2E_OIDC_PORT (default 8943),
# which the cluster trusts. Needs openssl and python3.
# KUBEHENGE_E2E_DATASTORE_ALLOCATE=1 switches the CSI plugin's newest release on and off again, on a
# KUBEHENGE_E2E_STORAGE that the management cluster, the templates and other clusters leave alone,
# with a management cluster that lets clusters choose it; KUBEHENGE_E2E_LOCAL_STORAGE names one of
# the management cluster's storages, which a cluster asking for it is refused.
# KUBEHENGE_E2E_BACKUP_TARGET names a backup target the management cluster was set up with whose
# endpoint is on this machine, at an https:// address the cluster's nodes reach; the test serves
# SeaweedFS's S3 gateway there with KUBEHENGE_E2E_BACKUP_CERT and KUBEHENGE_E2E_BACKUP_KEY, which
# KUBEHENGE_E2E_BACKUP_CA, the target's CA, signed.
# KUBEHENGE_E2E_HA_GROUPS=1 switches the cloud controller manager's HA group labels on and off again,
# with a management cluster that lets clusters choose them; the test makes an HA node-affinity rule
# over every node, which needs Proxmox VE 9, and removes it again.
set -eu

class=${1:?usage: $0 <ClusterClass> <version> <upgrade version>}
version=${2:?usage: $0 <ClusterClass> <version> <upgrade version>}
upgrade=${3:?usage: $0 <ClusterClass> <version> <upgrade version>}
: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_ADDRESSES:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}"
: "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_ENDPOINT:?}" "${KUBEHENGE_E2E_STORAGE:?}" "${KUBEHENGE_E2E_OIDC_ADDRESS:?}"
: "${KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES:?}"
: "${KUBEHENGE_E2E_BACKUP_TARGET:?}" "${KUBEHENGE_E2E_BACKUP_CA:?}" "${KUBEHENGE_E2E_BACKUP_CERT:?}" "${KUBEHENGE_E2E_BACKUP_KEY:?}"
load_balancer=${KUBEHENGE_E2E_LOAD_BALANCER:-metallb}

# The S3 store the backups go to.
# renovate: datasource=github-release-attachments depName=seaweedfs/seaweedfs
SEAWEEDFS_VERSION=4.48
SEAWEEDFS_SHA256=4a7d108384d044d95212d1342cdda9533fa55842c1c9b41f606ca3c8a9561124

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"
# shellcheck source=cis-benchmark.sh
. "$(dirname "$0")/cis-benchmark.sh"

work=$(mktemp -d)
distribution=$(echo "${class}" | sed 's/^kubehenge-//')
suffix=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
name=e2e-${distribution}-${suffix}
ns=${name}
issuer_pid=
grafana_pid=
s3_pid=
# The HA rule the nodes are labelled with, and the guest it needs.
ha_rule=''
ha_vmid=''
ha_node=''
cleanup() {
    status=$?
    [ -z "${issuer_pid}" ] || kill "${issuer_pid}" 2> /dev/null || true
    [ -z "${grafana_pid}" ] || kill "${grafana_pid}" 2> /dev/null || true
    [ -z "${s3_pid}" ] || kill "${s3_pid}" 2> /dev/null || true
    # shellcheck disable=SC2310 # each removal is tried, whatever happened to those before it
    remove_ha_rule || true
    if [ "${status}" -ne 0 ]; then
        printf '\nleaving cluster %s/%s for inspection; delete it with: kubectl delete namespace %s\n' "${ns}" "${name}" "${ns}"
    fi
    rm -rf "${work}"
}
trap cleanup EXIT

curl_ca=
[ -n "${KUBEHENGE_PROXMOX_CA:-}" ] && curl_ca="--cacert ${KUBEHENGE_PROXMOX_CA}"
# shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
pve() { curl -sS --fail ${curl_ca} -H "Authorization: PVEAPIToken=${KUBEHENGE_PROXMOX_TOKEN}" "$@"; }
pve_get() { pve "${KUBEHENGE_PROXMOX_URL}/api2/json/$1"; }
# pve_post <path> <curl options...>, pve_delete <path>: change Proxmox VE as its administrator.
pve_post() {
    api_path=$1
    shift
    pve -X POST "$@" "${KUBEHENGE_PROXMOX_URL}/api2/json/${api_path}" > /dev/null
}
pve_delete() { pve -X DELETE "${KUBEHENGE_PROXMOX_URL}/api2/json/$1" > /dev/null; }
# remove_ha_rule: takes the HA rule the nodes are labelled with, its resource and guest away again,
# in the order Proxmox VE allows.
remove_ha_rule() {
    [ -n "${ha_vmid}" ] || return 0
    pve_delete "cluster/ha/rules/${ha_rule}"
    pve_delete "cluster/ha/resources/vm:${ha_vmid}"
    pve_delete "nodes/${ha_node}/qemu/${ha_vmid}?purge=1"
    ha_vmid=''
}
wl() { KUBECONFIG="${work}/workload" kubectl "$@"; }
json_list() { printf '%s' "$1" | jq -R 'split(",")'; }
b64url() { base64 -w0 | tr '+/' '-_' | tr -d '='; }

step "an OpenID issuer for the cluster to trust"
issuer="https://${KUBEHENGE_E2E_OIDC_ADDRESS}:${KUBEHENGE_E2E_OIDC_PORT:-8943}"
mkdir -p "${work}/issuer/.well-known"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=kubehenge-e2e-ca \
    -keyout "${work}/ca.key" -out "${work}/ca.crt" 2> /dev/null
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=kubehenge-e2e-issuer \
    -keyout "${work}/tls.key" -out "${work}/tls.csr" 2> /dev/null
printf 'subjectAltName = IP:%s\nextendedKeyUsage = serverAuth\n' "${KUBEHENGE_E2E_OIDC_ADDRESS}" > "${work}/san.cnf"
openssl x509 -req -in "${work}/tls.csr" -CA "${work}/ca.crt" -CAkey "${work}/ca.key" -CAcreateserial -CAserial "${work}/ca.srl" \
    -days 1 -extfile "${work}/san.cnf" -out "${work}/tls.crt" 2> /dev/null
openssl genrsa -out "${work}/signing.key" 2048 2> /dev/null
openssl genrsa -out "${work}/other.key" 2048 2> /dev/null
modulus=$(openssl rsa -in "${work}/signing.key" -noout -modulus | cut -d= -f2)
n=$(python3 -c 'import sys; sys.stdout.buffer.write(bytes.fromhex(sys.argv[1]))' "${modulus}" | b64url)
printf '{"keys":[{"kty":"RSA","alg":"RS256","use":"sig","kid":"e2e","n":"%s","e":"AQAB"}]}' "${n}" > "${work}/issuer/jwks.json"
jq -n --arg i "${issuer}" '{issuer: $i, jwks_uri: ($i + "/jwks.json"), authorization_endpoint: ($i + "/auth"),
    response_types_supported: ["id_token"], subject_types_supported: ["public"], id_token_signing_alg_values_supported: ["RS256"]}' \
    > "${work}/issuer/.well-known/openid-configuration"
python3 -c '
import http.server, ssl, sys, functools
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=sys.argv[1])
s = http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[2])), handler)
c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); c.load_cert_chain(sys.argv[3], sys.argv[4])
s.socket = c.wrap_socket(s.socket, server_side=True)
s.serve_forever()' "${work}/issuer" "${KUBEHENGE_E2E_OIDC_PORT:-8943}" "${work}/tls.crt" "${work}/tls.key" > "${work}/issuer.log" 2>&1 &
issuer_pid=$!
until_true 30 "the issuer answers" curl -sS --fail --cacert "${work}/ca.crt" -o /dev/null "${issuer}/.well-known/openid-configuration"

# id_token <audience> <key>: an ID token of the test issuer for e2e@example.org in group e2e.
id_token() {
    now=$(date +%s)
    head=$(printf '{"alg":"RS256","kid":"e2e","typ":"JWT"}' | b64url)
    body=$(jq -cn --arg i "${issuer}" --arg a "$1" --argjson t "${now}" \
        '{iss: $i, aud: $a, sub: "e2e", email: "e2e@example.org", email_verified: true, groups: ["e2e"], iat: $t, exp: ($t + 600)}' | b64url)
    sig=$(printf '%s.%s' "${head}" "${body}" | openssl dgst -sha256 -sign "$2" | b64url)
    printf '%s.%s.%s' "${head}" "${body}" "${sig}"
}
# signed_in <token>: who the cluster's API server takes the token's bearer for, or nothing when it
# refuses the token. Any other outcome fails the test, so an API server that cannot be reached never
# passes for one that refuses.
signed_in() {
    server=$(wl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    if out=$(kubectl --kubeconfig=/dev/null --server="${server}" \
        --certificate-authority="${work}/workload-ca" --token="$1" auth whoami -o json 2>&1); then
        printf '%s' "${out}" | jq -c '{user: .status.userInfo.username, groups: [.status.userInfo.groups[] | select(startswith("oidc:"))]}'
        return
    fi
    case ${out} in
        *"You must be logged in to the server (Unauthorized)"*) ;;
        *) fail "asking the API server who a token's bearer is: ${out}" ;;
    esac
}
check_sign_in() {
    wl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "${work}/workload-ca"
    until_true 600 "the API server is ready" sh -c "KUBECONFIG=${work}/workload kubectl get --raw /readyz >/dev/null 2>&1"
    # Requests without credentials reach kubeadm's health checks, which kubeadm needs, and nothing
    # else; on RKE2 and k3s, nothing at all.
    server=$(wl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    for path in /readyz /version; do
        code=$(curl -sS --cacert "${work}/workload-ca" -o /dev/null -w '%{http_code}' "${server}${path}")
        case ${distribution}${path} in
            kubeadm/readyz) want=200 ;;
            *) want=401 ;;
        esac
        [ "${code}" = "${want}" ] || fail "a request without credentials to ${path} got ${code}, not ${want}"
    done
    # Each answer is assigned before it is tested: a failure inside a substitution within a test
    # would go unnoticed, while a failed assignment stops the test.
    token=$(id_token kubehenge-e2e "${work}/signing.key")
    got=$(signed_in "${token}")
    [ "${got}" = '{"user":"oidc:e2e@example.org","groups":["oidc:e2e"]}' ] || fail "an ID token signed in as ${got}"
    token=$(id_token someone-else "${work}/signing.key")
    got=$(signed_in "${token}")
    [ -z "${got}" ] || fail "a token for another audience was accepted"
    token=$(id_token kubehenge-e2e "${work}/other.key")
    got=$(signed_in "${token}")
    [ -z "${got}" ] || fail "a token signed by another key was accepted"
    # shellcheck disable=SC2310 # the account is there from the check before the upgrade; wl wraps one kubectl call.
    wl -n default create serviceaccount e2e-oidc > /dev/null 2>&1 || true
    token=$(wl -n default create token e2e-oidc --duration=10m)
    got=$(signed_in "${token}")
    bearer=$(printf '%s' "${got}" | jq -r .user)
    [ "${bearer}" = system:serviceaccount:default:e2e-oidc ] ||
        fail "ServiceAccount tokens no longer work beside the identity provider"
}

# cluster_network: the Cluster's clusterNetwork, when the test sets pod or service ranges.
cluster_network() {
    [ -n "${KUBEHENGE_E2E_PODS:-}${KUBEHENGE_E2E_SERVICES:-}" ] || return 0
    echo "  clusterNetwork:"
    if [ -n "${KUBEHENGE_E2E_PODS:-}" ]; then
        echo "    pods: {cidrBlocks: [${KUBEHENGE_E2E_PODS}]}"
    fi
    if [ -n "${KUBEHENGE_E2E_SERVICES:-}" ]; then
        echo "    services: {cidrBlocks: [${KUBEHENGE_E2E_SERVICES}]}"
    fi
}

step "create ${ns}/${name} from ${class} at ${version}"
kubectl create namespace "${ns}"
network_spec=$(cluster_network)
addresses=$(json_list "${KUBEHENGE_E2E_ADDRESSES}")
dns_servers=$(json_list "${KUBEHENGE_E2E_DNS}")
load_balancer_addresses=$(json_list "${KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES}")
cni_variable=
if [ -n "${KUBEHENGE_E2E_CNI:-}" ]; then
    cni_variable=$(printf '      - name: cni\n        value: %s\n' "${KUBEHENGE_E2E_CNI}")
fi
oidc=$(jq -cn --arg i "${issuer}" --rawfile ca "${work}/ca.crt" '[{issuerURL: $i, clientID: "kubehenge-e2e", certificateAuthority: $ca}]')
class_json=$(kubectl -n kubehenge-system get clusterclass "${class}" -o json)
# Unless set, the worker gets the least Kubehenge lets it have with every add-on but Rancher on, so
# that the add-ons are seen to fit in it: its distribution's least, or what a worker uses itself
# and every add-on's figure, whichever is more.
worker_memory=${KUBEHENGE_E2E_WORKER_MEMORY:-$(printf '%s' "${class_json}" | jq -r '
    (.metadata.annotations["kubehenge.dev/addon-memory"] | fromjson) as $addon
    | (.metadata.annotations["kubehenge.dev/node-memory"] | fromjson) as $node
    | [(.spec.variables[] | select(.name == "workerMachine") | .schema.openAPIV3Schema.properties.memoryMiB.minimum),
        $node.worker + $addon.headlamp + $addon.monitoring + $addon.logs + $addon.backups + $addon.kyverno + $addon.istio + $addon.loadBalancer + $addon.certManager + $addon.rancherAgent]
    | max')}
echo "workers with ${worker_memory} MiB of memory"
cat > "${work}/cluster.yaml" << EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: ${name}
  namespace: ${ns}
spec:
${network_spec}
  topology:
    classRef:
      name: ${class}
      namespace: kubehenge-system
    version: ${version}
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
      # From the start: on k3s, switching it on later replaces the control planes.
      - name: loadBalancer
        value: {enabled: true, implementation: ${load_balancer}, addresses: ${load_balancer_addresses}}
      - name: controlPlaneMachine
        value: {memoryMiB: ${KUBEHENGE_E2E_CONTROL_PLANE_MEMORY:-4096}, cores: ${KUBEHENGE_E2E_CONTROL_PLANE_CORES:-2}}
      - name: workerMachine
        value: {memoryMiB: ${worker_memory}}
${cni_variable}
      - name: oidc
        value: ${oidc}
      # The strictest level from the start, so that every component Kubehenge installs is seen to
      # run under it, or to sit in a namespace it exempts, and every add-on to work hardened.
      - name: podSecurity
        value: {admission: restricted}
      - name: cis
        value: true
EOF
kubectl apply -f "${work}/cluster.yaml"
# capmox does not come back to a machine whose VM failed to provision, so the wait ends there rather
# than at its timeout.
available_or_failed() {
    if kubectl -n "${ns}" get proxmoxmachines -l "cluster.x-k8s.io/cluster-name=${name}" -o json |
        jq -e '[.items[].status | .. | objects | select(.reason? == "VMProvisionFailed")] | length > 0' > /dev/null; then
        kubectl -n "${ns}" get proxmoxmachines -l "cluster.x-k8s.io/cluster-name=${name}" -o yaml >&2
        fail "a VM of ${name} failed to provision"
    fi
    kubectl -n "${ns}" wait --for=condition=Available "cluster/${name}" --timeout=1s > /dev/null 2>&1
}
until_true 1200 "cluster ${name} available" available_or_failed
kubectl -n "${ns}" get secret "${name}-kubeconfig" -o jsonpath='{.data.value}' | base64 -d > "${work}/workload"

step "the cluster's own Proxmox VE pool, user and volume owner"
creds=$(kubectl -n "${ns}" get secret "${name}-proxmox-token" -o json | jq -c '.data | map_values(@base64d)')
owner=$(printf '%s' "${creds}" | jq -r .volumeOwner)
user=$(printf '%s' "${creds}" | jq -r '.tokenID | split("!")[0]')
pool=$(pve_get pools | jq -r --arg n "${ns}.${name}" '.data[] | select(.poolid | endswith("/" + $n)) | .poolid')
[ -n "${pool}" ] || fail "no pool for the cluster"
pool_uri=$(jq -rn --arg p "${pool}" '$p | @uri')
members=$(pve_get "pools?poolid=${pool_uri}" | jq -c '[.data[0].members[] | select(.type == "qemu")]')
printf '%s' "${members}" | jq -e --argjson o "${owner}" '[.[] | select(.vmid == $o and .status == "stopped")] | length == 1' > /dev/null ||
    fail "volume owner VM ${owner} is not a stopped VM in pool ${pool}"
member_count=$(printf '%s' "${members}" | jq length)
[ "${member_count}" = 3 ] || fail "pool ${pool} holds ${member_count} VMs, not two nodes and the volume owner"
token_auth="Authorization: PVEAPIToken=$(printf '%s' "${creds}" | jq -r '.tokenID + "=" + .tokenSecret')"
# shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
cluster_get() { curl -sS --fail ${curl_ca} -H "${token_auth}" "${KUBEHENGE_PROXMOX_URL}/api2/json/$1"; }
seen=$(cluster_get "cluster/resources?type=vm" | jq '[.data[].vmid] | sort')
expected=$(printf '%s' "${members}" | jq '[.[].vmid] | sort')
[ "${seen}" = "${expected}" ] || fail "the cluster's token sees VMs ${seen}, not its own ${expected}"
storage_permissions=$(cluster_get "access/permissions?path=/storage/${KUBEHENGE_E2E_STORAGE}")
printf '%s' "${storage_permissions}" | jq -e '[.data[] | has("Datastore.Allocate")] | any | not' > /dev/null ||
    fail "the cluster's token may delete any volume on ${KUBEHENGE_E2E_STORAGE}"

step "sign-in through the cluster's identity provider, tokens beside it"
check_sign_in

step "the cluster uses its pod and service ranges"
# The Cluster's own, or else the class's defaults, as Kubehenge's controller reads them.
ranges=$(kubectl -n "${ns}" get "cluster/${name}" -o json | jq -c --argjson class "${class_json}" '{
    pods: (.spec.clusterNetwork.pods.cidrBlocks // [$class.metadata.annotations["kubehenge.dev/default-pods"]]),
    services: (.spec.clusterNetwork.services.cidrBlocks // [$class.metadata.annotations["kubehenge.dev/default-services"]])}')
# outside <ranges JSON list> <addresses...>: prints the addresses outside every range.
outside() { python3 -c 'import ipaddress, json, sys
nets = [ipaddress.ip_network(n) for n in json.loads(sys.argv[1])]
print(" ".join(a for a in sys.argv[2:] if not any(ipaddress.ip_address(a) in n for n in nets)))' "$@"; }
until_true 600 "pods running with addresses" sh -c "[ -n \"\$(KUBECONFIG=${work}/workload kubectl get pods -A -o json | jq -r '.items[] | select(.spec.hostNetwork != true and .status.podIP != null) | .status.podIP')\" ]"
service_ip=$(wl -n default get service kubernetes -o jsonpath='{.spec.clusterIP}')
service_ranges=$(printf '%s' "${ranges}" | jq -c .services)
stray=$(outside "${service_ranges}" "${service_ip}")
[ -z "${stray}" ] || fail "the kubernetes Service's ${service_ip} is outside ${ranges}"
pod_ranges=$(printf '%s' "${ranges}" | jq -c .pods)
pod_ips=$(wl get pods -A -o json | jq -r '.items[] | select(.spec.hostNetwork != true and .status.podIP != null) | .status.podIP')
# shellcheck disable=SC2086 # one address per word
stray=$(outside "${pod_ranges}" ${pod_ips})
[ -z "${stray}" ] || fail "pods have addresses ${stray} outside ${ranges}"

step "nodes initialised by the Proxmox VE cloud controller manager"
until_true 900 "two nodes ready" sh -c "[ \"\$(KUBECONFIG=${work}/workload kubectl get nodes -o json | jq '[.items[] | select(any(.status.conditions[]; .type == \"Ready\" and .status == \"True\"))] | length')\" = 2 ]"
until_true 600 "nodes initialised" sh -c "KUBECONFIG=${work}/workload kubectl get nodes -o json | jq -e '[.items[] | .spec.taints // [] | .[] | select(.key == \"node.cloudprovider.kubernetes.io/uninitialized\")] | length == 0' >/dev/null"
step "control-plane nodes tainted once a worker has joined"
until_true 180 "control planes tainted" sh -c "KUBECONFIG=${work}/workload kubectl get nodes -l node-role.kubernetes.io/control-plane -o json | jq -e '.items | length > 0 and all(.spec.taints // [] | any(.key == \"node-role.kubernetes.io/control-plane\" and .effect == \"NoSchedule\"))' >/dev/null"
nodes=$(wl get nodes -o json)
printf '%s' "${nodes}" | jq -e '.items | all(.spec.providerID | startswith("proxmox://")) and all(.metadata.labels["topology.kubernetes.io/region"] != null and .metadata.labels["topology.kubernetes.io/zone"] != null)' > /dev/null ||
    fail "nodes lack provider IDs or topology labels"
# Without an InternalIP the API server cannot reach the kubelets: no logs, exec or port-forward.
printf '%s' "${nodes}" | jq -e '.items | all(any(.status.addresses[]; .type == "InternalIP"))' > /dev/null ||
    fail "nodes have no InternalIP"
wl get nodes -L topology.kubernetes.io/region,topology.kubernetes.io/zone

step "the nodes pass the CIS Kubernetes Benchmark"
cis_benchmark

step "the cluster runs its CNI, which enforces network policies"
cni=$(kubectl -n "${ns}" get "cluster/${name}" -o json | jq -r '.spec.topology.variables[] | select(.name == "cni") | .value')
case ${cni} in
    cilium) wl -n kube-system rollout status daemonset/cilium --timeout=10m ;;
    calico) until_true 600 "calico-node" sh -c "KUBECONFIG=${work}/workload kubectl -n calico-system rollout status daemonset/calico-node --timeout=10s >/dev/null 2>&1" ;;
    canal) wl -n kube-system rollout status daemonset/rke2-canal --timeout=10m ;;
    # k3s's flannel runs inside k3s itself.
    flannel) ;;
    *) fail "no check for CNI ${cni}" ;;
esac
case ${cni} in
    cilium | calico)
        nodes=$(wl get nodes -o json)
        printf '%s' "${nodes}" | jq -e '.items | all(.metadata.annotations | keys | any(startswith("flannel.alpha.coreos.com/")) | not)' > /dev/null ||
            fail "flannel runs beside ${cni}"
        ;;
    # Canal and k3s's own CNI carry flannel by design.
    *) ;;
esac
# agnhost_pod <namespace> <name> <args...>: an agnhost pod at Pod Security's restricted level,
# which the cluster enforces; the image runs as root unless told otherwise. The overrides merge into
# the container by its name. sh has no local variables, so its own carry its prefix.
agnhost_pod() {
    agnhost_overrides=$(printf '{"apiVersion":"v1","spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"%s","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' "$2")
    agnhost_namespace=$1 agnhost_name=$2
    shift 2
    wl -n "${agnhost_namespace}" run "${agnhost_name}" --image="${agnhost}" --override-type=strategic --overrides="${agnhost_overrides}" -- "$@"
}
wl create namespace e2e-policy
agnhost_pod e2e-policy server netexec --http-port=8080
agnhost_pod e2e-policy client pause
wl -n e2e-policy wait --for=condition=Ready pod/server pod/client --timeout=5m
server_ip=$(wl -n e2e-policy get pod server -o jsonpath='{.status.podIP}')
connect() { KUBECONFIG=${work}/workload kubectl -n e2e-policy exec client -- /agnhost connect "${server_ip}:8080" --timeout=3s 2>&1; }
reaches() { connect > /dev/null; }
# Calico and Cilium drop the packets, so the attempt times out; k3s's own policy controller
# (kube-router) rejects them, so it is refused, which with the server still running is the policy
# too. Any other failure, a failing exec among them, does not count as blocked.
blocked() { connect | grep -qxE 'TIMEOUT|REFUSED'; }
until_true 120 "the client reaches the server" reaches
wl -n e2e-policy apply -f - << 'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-ingress
spec:
  podSelector: {}
  policyTypes: [Ingress]
EOF
until_true 120 "the policy blocks the client" blocked
# shellcheck disable=SC2310 # wl wraps one kubectl call, whose status is the one tested.
wl -n e2e-policy wait --for=condition=Ready pod/server --timeout=10s > /dev/null ||
    fail "the server stopped; the refusal was not the policy's"
wl delete namespace e2e-policy --wait=false

step "the health page's identities see a crash loop, and nothing they need not"
# identity_kubeconfig <health | health-logs>: a kubeconfig of the token Kubehenge's controller keeps
# beside the cluster, written to a file only this user reads rather than passed as an argument.
identity_kubeconfig() {
    identity_secret=$(kubectl -n "${ns}" get "secret/${name}-$1" -o json 2> /dev/null) || return 1
    (
        umask 077
        printf '%s' "${identity_secret}" | jq '{apiVersion: "v1", kind: "Config", "current-context": "c",
            clusters: [{name: "c", cluster: {server: (.data.server | @base64d), "certificate-authority-data": .data["ca.crt"]}}],
            users: [{name: "u", user: {token: (.data.token | @base64d)}}],
            contexts: [{name: "c", context: {cluster: "c", user: "u"}}]}' > "${work}/$1"
    )
}
until_true 300 "the health identities' tokens beside the cluster" identity_kubeconfig health
identity_kubeconfig health-logs
# refused <identity> <can-i arguments...>: the identity may not do it.
refused() {
    refused_identity=$1
    shift
    refused_answer=$(KUBECONFIG="${work}/${refused_identity}" kubectl auth can-i "$@" 2> /dev/null || true)
    [ "${refused_answer}" = no ] || fail "${refused_identity} may $*"
}
wl create namespace e2e-health
# An unknown command makes agnhost exit at once, so the pod restarts until it backs off.
agnhost_pod e2e-health crasher no-such-command
crash_seen() {
    crash_pod=$(KUBECONFIG="${work}/health" kubectl -n e2e-health get pod crasher -o json 2> /dev/null) || return 1
    printf '%s' "${crash_pod}" | jq -e '.status.containerStatuses[0].state.waiting.reason == "CrashLoopBackOff"' > /dev/null
}
until_true 300 "the health identity sees the pod crash-looping" crash_seen
health_ready=$(KUBECONFIG="${work}/health" kubectl get --raw /readyz) ||
    fail "the health identity cannot read the API server's readiness"
[ "${health_ready}" = ok ] || fail "the API server is not ready: ${health_ready}"
refused health get secrets -n kube-system
refused health get pods --subresource=log -n e2e-health
refused health create pods -n e2e-health
refused health delete nodes
refused health-logs list pods -n e2e-health
refused health-logs get secrets -n kube-system
# Raw, as kubectl logs would first read the pod, which this identity may not. Without previous=true,
# as the health page reads: the kubelet hands out the newest run, the crash, which it keeps, while
# previous=true can name a run it has already removed.
crash_log=$(KUBECONFIG="${work}/health-logs" kubectl get --raw "/api/v1/namespaces/e2e-health/pods/crasher/log") ||
    fail "the logs identity cannot read the pod's log"
printf '%s' "${crash_log}" | grep -q no-such-command || fail "the pod's log does not tell why it stopped: ${crash_log}"
wl delete namespace e2e-health --wait=false

step "the ingress controller serves a host name on the API address and every node"
wl create namespace e2e-ingress
agnhost_pod e2e-ingress web netexec --http-port=8080
wl -n e2e-ingress expose pod web --port=80 --target-port=8080
wl -n e2e-ingress create ingress web --rule='e2e.test/*=web:80'
wl -n e2e-ingress wait --for=condition=Ready pod/web --timeout=5m
# served <address> [host name]: whether the address serves the web pod at the host name, e2e.test
# unless given; netexec answers /hostname with its pod's name.
served() {
    served_answer=$(curl -sS --max-time 5 -H "Host: ${2:-e2e.test}" "http://$1/hostname" 2> /dev/null) || return 1
    [ "${served_answer}" = web ]
}
# The Gateway API's objects, as <kind>/<name>: its CRDs and the admission policy beside them.
gateway_api_version=$(sed -n 's/^GATEWAY_API_TAG := //p' "$(dirname "$0")/../../Makefile")
gateway_api_objects=
for gateway_api_crd in backendtlspolicies gatewayclasses gateways grpcroutes httproutes listenersets referencegrants tcproutes tlsroutes udproutes; do
    gateway_api_objects="${gateway_api_objects} CustomResourceDefinition/${gateway_api_crd}.gateway.networking.k8s.io"
done
gateway_api_objects="${gateway_api_objects} ValidatingAdmissionPolicy/safe-upgrades.gateway.networking.k8s.io ValidatingAdmissionPolicyBinding/safe-upgrades.gateway.networking.k8s.io"
# gateway_api_say <reason>: says why the Gateway API is not as it should be yet, once per reason,
# as the checks below are polled.
gateway_api_say() {
    [ "$1" = "${gateway_api_reason:-}" ] || printf 'the Gateway API: %s\n' "$1" >&2
    gateway_api_reason=$1
}
# gateway_api_shipped: the Gateway API's objects the latest revision of every Helm release ships,
# where that revision is deployed, one per line as "<namespace>/<release> <kind> <name> <Gateway API
# release>". Helm keeps a release gzipped and in base64, within the Secret's own base64.
gateway_api_shipped() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
    gateway_api_revisions=$(wl get secret -A -l owner=helm -o json) || return 1
    printf '%s' "${gateway_api_revisions}" |
        jq -r '.items | group_by(.metadata.namespace + "/" + .metadata.labels.name) | map(max_by(.metadata.labels.version | tonumber)) | .[]
            | select(.metadata.labels.status == "deployed") | "\(.metadata.namespace)/\(.metadata.labels.name) \(.data.release)"' |
        while read -r gateway_api_release gateway_api_stored; do
            printf '%s' "${gateway_api_stored}" | base64 -d | base64 -d | gzip -dc | jq -r .manifest |
                awk -v release="${gateway_api_release}" '
                    function emit() {
                        if (kind != "" && name ~ /\.gateway\.networking\.k8s\.io$/) print release, kind, name, (version == "" ? "none" : version)
                    }
                    /^---/ { emit(); kind = ""; name = ""; version = ""; meta = 0; next }
                    /^kind:/ { kind = $2; meta = 0; next }
                    /^metadata:/ { meta = 1; next }
                    /^[^ #]/ { meta = 0 }
                    meta && /^  name:/ { name = $2; gsub(/"/, "", name) }
                    meta && /^    gateway\.networking\.k8s\.io\/bundle-version:/ { version = $2; gsub(/"/, "", version) }
                    END { emit() }' || exit 1
        done
}
# gateway_api_held: every object of the Gateway API is held once. One that a Helm release ships is
# that release's alone: no other release ships it, its annotations name that release, and it is of
# the Gateway API release the chart ships, which nothing has replaced. One that no Helm release
# ships is of Kubehenge's release or newer.
gateway_api_held() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
    gateway_api_shipping=$(gateway_api_shipped) || return 1
    for gateway_api_object in ${gateway_api_objects}; do
        gateway_api_kind=${gateway_api_object%%/*} gateway_api_name=${gateway_api_object#*/}
        # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
        gateway_api_has=$(wl get "${gateway_api_kind}" "${gateway_api_name}" -o json 2> /dev/null) || return 1
        gateway_api_named=$(printf '%s' "${gateway_api_has}" | jq -r '.metadata.annotations // {} | "\(.["meta.helm.sh/release-namespace"] // "")/\(.["meta.helm.sh/release-name"] // "")"')
        gateway_api_is=$(printf '%s' "${gateway_api_has}" | jq -r '.metadata.annotations["gateway.networking.k8s.io/bundle-version"] // "none"')
        gateway_api_holders=$(printf '%s\n' "${gateway_api_shipping}" | awk -v k="${gateway_api_kind}" -v n="${gateway_api_name}" '$2 == k && $3 == n')
        gateway_api_count=$(printf '%s' "${gateway_api_holders}" | grep -c . || true)
        case ${gateway_api_count} in
            0)
                gateway_api_oldest=$(printf '%s\n%s\n' "${gateway_api_is}" "${gateway_api_version}" | sort -V | head -n 1)
                [ "${gateway_api_is}" != none ] && [ "${gateway_api_oldest}" = "${gateway_api_version}" ] || {
                    gateway_api_say "${gateway_api_object} is of release ${gateway_api_is}, which no Helm release ships, not ${gateway_api_version} or newer"
                    return 1
                }
                ;;
            1)
                gateway_api_holder=${gateway_api_holders%% *} gateway_api_ships=${gateway_api_holders##* }
                [ "${gateway_api_named}" = "${gateway_api_holder}" ] || {
                    gateway_api_say "${gateway_api_object} is shipped by ${gateway_api_holder} but marked as ${gateway_api_named}'s"
                    return 1
                }
                [ "${gateway_api_is}" = "${gateway_api_ships}" ] || {
                    gateway_api_say "${gateway_api_object} is of release ${gateway_api_is}, where ${gateway_api_holder} ships ${gateway_api_ships}"
                    return 1
                }
                ;;
            *)
                gateway_api_releases=$(printf '%s\n' "${gateway_api_holders}" | cut -d ' ' -f 1 | tr '\n' ' ')
                gateway_api_say "${gateway_api_object} is shipped by more than one Helm release: ${gateway_api_releases}"
                return 1
                ;;
        esac
    done
}
# The Gateway API chart of the distribution's own that the releases the test upgrades RKE2 and k3s
# to have, which adopts what the Traefik CRD chart of the release before left behind.
case ${distribution} in
    rke2) gateway_api_chart=rke2-gateway-api-crd ;;
    k3s) gateway_api_chart=gateway-api-crd ;;
    *) gateway_api_chart= ;;
esac
# gateway_api_chart_holds: the distribution's own Gateway API chart is installed and ships every
# object of the Gateway API.
gateway_api_chart_holds() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
    wl -n kube-system get helmchart "${gateway_api_chart}" > /dev/null 2>&1 || {
        gateway_api_say "the cluster has no HelmChart ${gateway_api_chart}"
        return 1
    }
    # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
    gateway_api_shipping=$(gateway_api_shipped) || return 1
    for gateway_api_object in ${gateway_api_objects}; do
        printf '%s\n' "${gateway_api_shipping}" | awk -v r="kube-system/${gateway_api_chart}" -v k="${gateway_api_object%%/*}" -v n="${gateway_api_object#*/}" \
            '$1 == r && $2 == k && $3 == n { found = 1 } END { exit !found }' || {
            gateway_api_say "the latest revision of ${gateway_api_chart} is not deployed, or does not ship ${gateway_api_object}"
            return 1
        }
    done
}
gateway_programmed() {
    wl -n kube-system get gateway traefik-gateway -o json 2> /dev/null |
        jq -e --arg ip "${KUBEHENGE_E2E_ENDPOINT}" 'any(.status.conditions[]?; .type == "Programmed" and .status == "True") and any(.status.addresses[]?; .value == $ip)' > /dev/null
}
node_addresses=$(wl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}')
for address in ${KUBEHENGE_E2E_ENDPOINT} ${node_addresses}; do
    until_true 300 "${address} serves the host name" served "${address}"
done

step "the Gateway API: Traefik's Gateway routes an HTTPRoute"
until_true 300 "the Gateway API's objects, each held once, by Kubehenge or a Helm release" gateway_api_held
wl get validatingadmissionpolicy,validatingadmissionpolicybinding safe-upgrades.gateway.networking.k8s.io > /dev/null
until_true 300 "Traefik's Gateway is programmed, at the API address" gateway_programmed
wl -n e2e-ingress apply -f - << 'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: web
spec:
  parentRefs:
    - name: traefik-gateway
      namespace: kube-system
      sectionName: web
  hostnames: [gateway.e2e.test]
  rules:
    - backendRefs:
        - name: web
          port: 80
EOF
wl -n e2e-ingress wait --for=jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}'=True httproute/web --timeout=2m
until_true 300 "the API address serves the route" served "${KUBEHENGE_E2E_ENDPOINT}" gateway.e2e.test
wl delete namespace e2e-ingress --wait=false

step "a Service of type LoadBalancer answers on an address from the cluster's range, with ${load_balancer}"
wl create namespace e2e-lb
agnhost_pod e2e-lb web netexec --http-port=8080
# Off port 80, which Traefik holds on every node, so that k3s's servicelb can answer for it later.
wl -n e2e-lb expose pod web --type=LoadBalancer --port=8080 --target-port=8080
wl -n e2e-lb wait --for=condition=Ready pod/web --timeout=5m
# shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
lb_ips() { lb_ip=$(wl -n e2e-lb get service web -o jsonpath='{.status.loadBalancer.ingress[*].ip}') && [ -n "${lb_ip}" ]; }
until_true 600 "the Service given an address" lb_ips
lb_ip=${lb_ip%% *}
# in_range <comma-separated ranges, networks or addresses> <address>: whether the address is in one.
in_range() { python3 -c 'import ipaddress, sys
a = ipaddress.ip_address(sys.argv[2])
def holds(e):
    if "-" in e:
        first, last = (ipaddress.ip_address(x.strip()) for x in e.split("-"))
        return first <= a <= last
    return a in ipaddress.ip_network(e.strip(), strict=False)
sys.exit(0 if any(holds(e) for e in sys.argv[1].split(",")) else 1)' "$@"; }
# shellcheck disable=SC2310 # the answer is the status.
in_range "${KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES}" "${lb_ip}" ||
    fail "the Service's ${lb_ip} is not from ${KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES}"
# netexec answers /hostname with its pod's name.
answers() {
    answer=$(curl -sS --max-time 5 "http://$1:8080/hostname" 2> /dev/null) || return 1
    [ "${answer}" = web ]
}
until_true 300 "${lb_ip} answers for the Service" answers "${lb_ip}"
if [ "${distribution}" = k3s ]; then
    servicelb=$(wl -n kube-system get daemonsets -l svccontroller.k3s.cattle.io/svcname -o name)
    [ -z "${servicelb}" ] || fail "k3s's servicelb runs beside the load balancer: ${servicelb}"
fi

owned() {
    first_node=$(pve_get nodes | jq -r '.data[0].node')
    pve_get "nodes/${first_node}/storage/${KUBEHENGE_E2E_STORAGE}/content?content=images&vmid=${owner}" | jq '.data | length'
}
# persistent_volume: a pod writes to a volume of its own, which goes again with its claim.
persistent_volume() {
    wl apply -f - << 'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: e2e
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e
  namespace: default
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: write
      # renovate: datasource=docker depName=registry.k8s.io/pause
      image: registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: e2e
EOF
    wl -n default wait --for=condition=Ready pod/e2e --timeout=10m
    pv=$(wl -n default get pvc e2e -o jsonpath='{.spec.volumeName}')
    volume=$(wl get pv "${pv}" -o jsonpath='{.spec.csi.volumeHandle}')
    printf 'volume %s\n' "${volume}"
    owned_volumes=$(owned)
    [ "${owned_volumes}" -ge 1 ] || fail "no volume owned by the volume owner VM ${owner}"
    wl -n default delete pod/e2e pvc/e2e --wait --timeout=5m
    # The CSI plugin retries a failed deletion with a back-off of up to five minutes.
    until_true 600 "volume deleted" sh -c "[ \"\$(KUBECONFIG=${work}/workload kubectl get pv -o name | wc -l)\" = 0 ]"
    owned_volumes=$(owned)
    [ "${owned_volumes}" = 0 ] || fail "the volume is still on ${KUBEHENGE_E2E_STORAGE}"
}

step "a persistent volume from the Proxmox VE CSI plugin"
persistent_volume

# set_variable <name> <JSON value>: sets one of the Cluster's topology variables.
set_variable() {
    vars=$(kubectl -n "${ns}" get "cluster/${name}" -o json |
        jq -c --arg n "$1" --argjson v "$2" '[.spec.topology.variables[] | select(.name != $n)] + [{name: $n, value: $v}]')
    kubectl -n "${ns}" patch "cluster/${name}" --type=merge -p "{\"spec\":{\"topology\":{\"variables\":${vars}}}}"
}

if [ "${KUBEHENGE_E2E_DATASTORE_ALLOCATE:-}" = 1 ]; then
    : "${KUBEHENGE_E2E_LOCAL_STORAGE:?}"
    # The plugin's two releases, by the version of the controller image the controller's chart
    # values name for each.
    controller_args=$(kubectl get deployments -A -o json |
        jq -c '[.items[].spec.template.spec.containers[].args // [] | select(any(startswith("--csi-newest-chart=")))][0]')
    release_of() {
        printf '%s' "${controller_args}" | jq -r --arg c "$1" '.[] | select(startswith("--chart-values=")) |
            sub("^--chart-values="; "") | fromjson | .[$c].controller.plugin.image.tag // "" | split("@")[0]'
    }
    newest=$(release_of csi-newest)
    held=$(release_of csi)
    [ -n "${newest}" ] && [ -n "${held}" ] && [ "${newest}" != "${held}" ] ||
        fail "the controller names no two releases of the CSI plugin: '${newest}' and '${held}'"
    # allocate_is <true|false>: whether the cluster's token holds Datastore.Allocate on its storage.
    allocate_is() {
        # shellcheck disable=SC2310 # polled; a failed read is a no.
        perms=$(cluster_get "access/permissions?path=/storage/${KUBEHENGE_E2E_STORAGE}") || return 1
        printf '%s' "${perms}" | jq -e --argjson want "$1" '([.data[] | has("Datastore.Allocate")] | any) == $want' > /dev/null
    }
    # csi_at <version>: every pod of the CSI plugin runs its images at the version, and is ready.
    csi_at() {
        wl get pods -A -o json | jq -e --arg v "$1" '
            def plugin: .image | test("/proxmox-csi-(controller|node):");
            [.items[] | select(any(.spec.containers[]; plugin))] |
            length > 0 and all(
                all(.spec.containers[] | select(plugin); .image | split("@")[0] | endswith(":" + $v)) and
                any(.status.conditions[]?; .type == "Ready" and .status == "True"))' > /dev/null
    }
    # refused_with <message> <storage> <csi value>: a second cluster beside this one, on the storage
    # and with the csi variable, is refused for the reason.
    refused_with() {
        other=$(kubectl -n "${ns}" get "cluster/${name}" -o json | jq -c --arg s "$2" --argjson c "$3" '
            {apiVersion, kind, metadata: {name: "e2e-refused", namespace: .metadata.namespace},
             spec: (.spec | del(.controlPlaneEndpoint) | .topology.variables = ([.topology.variables[] |
                 select(.name != "storage" and .name != "csi")] + [{name: "storage", value: $s}, {name: "csi", value: $c}]))}')
        if out=$(printf '%s' "${other}" | kubectl create --dry-run=server -f - 2>&1); then
            fail "a cluster on storage $2 with csi $3 was let in"
        fi
        printf '%s' "${out}" | grep -qF "$1" || fail "a cluster on storage $2 was refused for another reason: ${out}"
    }

    step "the CSI plugin's newest release, with Datastore.Allocate on the cluster's own storage"
    set_variable csi '{"datastoreAllocate":true}'
    until_true 300 "the cluster's token to hold Datastore.Allocate on ${KUBEHENGE_E2E_STORAGE}" allocate_is true
    until_true 900 "the CSI plugin at ${newest}" csi_at "${newest}"
    persistent_volume
    refused_with "holds Datastore.Allocate on storage ${KUBEHENGE_E2E_STORAGE}" "${KUBEHENGE_E2E_STORAGE}" '{"datastoreAllocate":false}'
    refused_with "the management cluster uses storage ${KUBEHENGE_E2E_LOCAL_STORAGE}" "${KUBEHENGE_E2E_LOCAL_STORAGE}" '{"datastoreAllocate":true}'

    step "the CSI plugin's held release again, without Datastore.Allocate"
    set_variable csi '{"datastoreAllocate":false}'
    until_true 300 "the cluster's token to lose Datastore.Allocate on ${KUBEHENGE_E2E_STORAGE}" allocate_is false
    until_true 900 "the CSI plugin at ${held}" csi_at "${held}"
    persistent_volume
fi

if [ "${KUBEHENGE_E2E_HA_GROUPS:-}" = 1 ]; then
    # audit_is <true|false>: whether the cluster's token holds Sys.Audit on /.
    audit_is() {
        # shellcheck disable=SC2310 # polled; a failed read is a no.
        perms=$(cluster_get "access/permissions?path=/") || return 1
        printf '%s' "${perms}" | jq -e --argjson want "$1" '([.data[] | has("Sys.Audit")] | any) == $want' > /dev/null
    }
    # labelled: every node carries the label of the test's HA rule.
    labelled() {
        wl get nodes -o json | jq -e --arg l "group.topology.proxmox.sinextra.dev/${ha_rule}" \
            '.items | length > 0 and all(.metadata.labels | has($l))' > /dev/null
    }
    # The guest exists once Proxmox VE answers for it.
    ha_guest_exists() { pve_get "nodes/${ha_node}/qemu/${ha_vmid}/status/current" > /dev/null 2>&1; }

    step "nodes labelled with their HA rule, with Sys.Audit on / for the cluster's token"
    # shellcheck disable=SC2310 # a failed read fails the step
    audit_is false || fail "the cluster's token holds Sys.Audit on / without asking for it"
    # A node-affinity rule over every node, for a guest of its own: Proxmox VE takes a rule only for
    # HA resources, and a resource only for a guest that exists.
    ha_node=$(pve_get nodes | jq -r '[.data[] | select(.status == "online") | .node][0]')
    ha_nodes=$(pve_get nodes | jq -r '[.data[] | select(.status == "online") | .node] | join(",")')
    ha_rule=${name}-labels
    ha_vmid=$(pve_get cluster/nextid | jq -r .data)
    pve_post "nodes/${ha_node}/qemu" --data-urlencode "vmid=${ha_vmid}" --data-urlencode "name=${ha_rule}" \
        --data-urlencode memory=64
    until_true 120 "guest ${ha_vmid}" ha_guest_exists
    # Ignored, so that Proxmox VE HA neither starts the guest nor arms a node's watchdog for it.
    pve_post cluster/ha/resources --data-urlencode "sid=vm:${ha_vmid}" --data-urlencode state=ignored
    pve_post cluster/ha/rules --data-urlencode type=node-affinity --data-urlencode "rule=${ha_rule}" \
        --data-urlencode "resources=vm:${ha_vmid}" --data-urlencode "nodes=${ha_nodes}"
    set_variable ccm '{"haGroups":true}'
    until_true 300 "the cluster's token to hold Sys.Audit on /" audit_is true
    # The cloud controller manager labels the nodes when it next syncs them.
    until_true 900 "every node labelled with HA rule ${ha_rule}" labelled
    wl get nodes -L "group.topology.proxmox.sinextra.dev/${ha_rule}"

    step "the cluster's token without Sys.Audit on / again"
    set_variable ccm '{"haGroups":false}'
    until_true 300 "the cluster's token to lose Sys.Audit on /" audit_is false
    remove_ha_rule
fi
# guest <node> <vmid> <command>: runs a shell command in a VM through its guest agent and prints its
# output; fails when the command or the agent does. It is polled with set -e off, so each step
# returns its own failure.
guest() {
    request=$(jq -cn --arg c "$3" '{command: ["sh", "-c", $c]}')
    # shellcheck disable=SC2310 # guest is polled with set -e off, so a failed request is returned.
    pid=$(pve -X POST -H 'Content-Type: application/json' -d "${request}" \
        "${KUBEHENGE_PROXMOX_URL}/api2/json/nodes/$1/qemu/$2/agent/exec" | jq -r .data.pid) || return 1
    tries=0
    # shellcheck disable=SC2310 # polled until the command has exited; a failed request is tried again.
    until result=$(pve_get "nodes/$1/qemu/$2/agent/exec-status?pid=${pid}") &&
        printf '%s' "${result}" | jq -e '.data.exited == 1' > /dev/null; do
        tries=$((tries + 1))
        [ "${tries}" -lt 60 ] || return 1
        sleep 1
    done
    printf '%s' "${result}" | jq -r '.data."out-data" // ""'
    printf '%s' "${result}" | jq -e '.data.exitcode == 0' > /dev/null
}
# on_machines "<Machine names>" <command>: runs the command on those Machines' VMs.
on_machines() {
    for m in $1; do
        pm=$(kubectl -n "${ns}" get machine "${m}" -o jsonpath='{.spec.infrastructureRef.name}')
        pm_node=$(kubectl -n "${ns}" get proxmoxmachine "${pm}" -o jsonpath='{.status.proxmoxNode}')
        pm_vmid=$(kubectl -n "${ns}" get proxmoxmachine "${pm}" -o jsonpath='{.spec.virtualMachineID}')
        guest "${pm_node}" "${pm_vmid}" "$2"
    done
}
# restarts: each pod's container restarts, by namespace/name.
restarts() {
    wl get pods -A -o json |
        jq -c '[.items[] | {key: "\(.metadata.namespace)/\(.metadata.name)", value: ([.status.containerStatuses[]?.restartCount] | add // 0)}] | from_entries'
}
# server_restarts: how often systemd has restarted the control planes' distribution service. k3s runs
# every control-plane component in it, so it is the one place k3s's restarts show.
case ${class} in
    kubehenge-rke2) server_unit=rke2-server ;;
    kubehenge-k3s) server_unit=k3s ;;
    *) server_unit=kubelet ;;
esac
servers=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
server_restarts() {
    # shellcheck disable=SC2310 # its failure is returned.
    counts=$(on_machines "${servers}" "systemctl show -p NRestarts --value ${server_unit}") || return 1
    printf '%s\n' "${counts}" | awk '{ n += $1 } END { print n + 0 }'
}
pods_before=$(restarts)
# shellcheck disable=SC2310 # a failed count fails the step.
servers_before=$(server_restarts) || fail "cannot read ${server_unit}'s restarts on the control planes"
echo "memory before the add-ons:"
pod_memory "${work}/workload"
# What the guests are told of the CPU decides which mitigations their kernels run, and those cost
# the most under nested virtualisation: the figure to read beside a slow run.
echo "the CPU a control plane sees:"
# shellcheck disable=SC2310 # a node that cannot be read is said so, not fatal here.
on_machines "${servers%% *}" "grep -m1 'model name' /proc/cpuinfo; grep -qw pcid /proc/cpuinfo && echo 'pcid: yes' || echo 'pcid: no'; grep -H . /sys/devices/system/cpu/vulnerabilities/*" ||
    echo "  cannot read it"

step "Headlamp switched on"
set_variable headlamp '{"enabled":true}'
until_true 600 "Headlamp installed" sh -c "KUBECONFIG=${work}/workload kubectl -n headlamp get deployment/headlamp >/dev/null 2>&1"
wl -n headlamp rollout status deployment/headlamp --timeout=10m
# Through the API server's service proxy, as Kubehenge's interface reaches it. A rollout counts the
# pod available before the endpoints controller has published it, and until then the proxy answers
# that the Service has no endpoints.
headlamp_page() {
    wl get --raw /api/v1/namespaces/headlamp/services/http:headlamp:80/proxy/ 2> /dev/null | grep -q -i '<html'
}
until_true 60 "Headlamp answers through the API server" headlamp_page
# shellcheck disable=SC2310 # can-i exits non-zero for its answer no, which the comparison reads.
can_list=$(wl auth can-i list pods --all-namespaces --as=system:serviceaccount:headlamp:headlamp || true)
[ "${can_list}" = no ] || fail "Headlamp's service account has rights in the cluster"
pod_security=$(wl label --dry-run=server --overwrite namespace headlamp pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "Headlamp's pod does not meet Pod Security's restricted level" ;;
    # Only a pod that falls short of the level gets a warning.
    *) ;;
esac

step "pod security enforced, with Kyverno switched on"
# privileged_pod <namespace>: asks the API server to admit a privileged pod, without creating it.
privileged_pod() {
    wl -n "$1" run e2e-privileged --image="${pause_image}" --restart=Never --dry-run=server \
        --overrides="{\"apiVersion\":\"v1\",\"spec\":{\"containers\":[{\"name\":\"e2e-privileged\",\"image\":\"${pause_image}\",\"securityContext\":{\"privileged\":true}}]}}" 2>&1
}
# shellcheck disable=SC2310 # the refusal is the answer looked for.
refused=$(privileged_pod default) && fail "a privileged pod is admitted under Pod Security's restricted level"
case ${refused} in
    *'violates PodSecurity "restricted'*) ;;
    *) fail "a privileged pod is refused for another reason: ${refused}" ;;
esac
# A namespace labelled privileged is past Pod Security Admission, but not past Kyverno.
wl create namespace e2e-privileged
wl label namespace e2e-privileged pod-security.kubernetes.io/enforce=privileged
set_variable podSecurity '{"admission":"restricted","kyverno":"restricted"}'
kyverno_refuses() {
    # shellcheck disable=SC2310 # admitted is a retry until the policies are in place.
    out=$(privileged_pod e2e-privileged) && return 1
    case ${out} in
        *disallow-privileged-containers*) ;;
        *) return 1 ;;
    esac
}
until_true 900 "Kyverno refuses a privileged pod" kyverno_refuses
pod_security=$(wl label --dry-run=server --overwrite namespace kyverno pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "Kyverno's pods do not meet Pod Security's restricted level: ${pod_security}" ;;
    *) ;;
esac

# Before monitoring: on Cilium, switching Istio on restarts Cilium's agents and operator, and with
# monitoring's pods busy beside them a worker of the least size runs short of CPU.
step "Istio switched on, the mesh's traffic over mutual TLS"
set_variable istio '{"enabled":true}'
until_true 900 "istiod available" wl -n istio-system wait --for=condition=Available deployment/istiod --timeout=10s
until_true 600 "ztunnel on every node" wl -n istio-node rollout status daemonset/ztunnel --timeout=10s
until_true 600 "istio-cni on every node" wl -n istio-node rollout status daemonset/istio-cni-node --timeout=10s
wl create namespace e2e-mesh
wl label namespace e2e-mesh istio.io/dataplane-mode=ambient
wl create namespace e2e-plain
agnhost_pod e2e-mesh server netexec --http-port=8080
agnhost_pod e2e-mesh client pause
agnhost_pod e2e-plain client pause
wl -n e2e-mesh wait --for=condition=Ready pod/server pod/client --timeout=5m
wl -n e2e-plain wait --for=condition=Ready pod/client --timeout=5m
redirected() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    marks=$(wl -n e2e-mesh get pod server client -o jsonpath='{.items[*].metadata.annotations.ambient\.istio\.io/redirection}') &&
        [ "${marks}" = "enabled enabled" ]
}
until_true 120 "the mesh's pods redirected through ztunnel" redirected
mesh_ip=$(wl -n e2e-mesh get pod server -o jsonpath='{.status.podIP}')
# fetch <namespace>: the server's host name, fetched over HTTP by the namespace's client.
fetch() { KUBECONFIG=${work}/workload kubectl -n "$1" exec client -- curl -sS --max-time 5 "http://${mesh_ip}:8080/hostname" 2>&1; }
# shellcheck disable=SC2310 # polled with set -e off, so a failed fetch is only a retry.
fetches() { fetched=$(fetch "$1") && [ "${fetched}" = server ]; }
until_true 60 "a client outside the mesh reaches the server while mutual TLS is optional" fetches e2e-plain
wl -n e2e-mesh apply -f - << 'EOF'
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: strict
spec:
  mtls:
    mode: STRICT
EOF
# ztunnel closes a plain-text connection the policy refuses, so curl gets no reply; any other
# failure, a failing exec among them, does not count as refused.
plain_refused() { fetch e2e-plain | grep -qE '^curl: \((52|56)\)'; }
until_true 120 "a client outside the mesh refused once mutual TLS is required" plain_refused
until_true 60 "a client in the mesh reaches the server" fetches e2e-mesh
# What ztunnel let through carries the client's workload identity.
mesh_identity() { wl -n istio-node logs -l app=ztunnel --tail=-1 --prefix=false | grep -q 'spiffe://cluster.local/ns/e2e-mesh/sa/default'; }
until_true 60 "ztunnel names the client's workload identity" mesh_identity

step "monitoring switched on"
set_variable monitoring '{"enabled":true}'
prometheus=statefulset/prometheus-kube-prometheus-stack-prometheus
until_true 1200 "Prometheus installed" sh -c "KUBECONFIG=${work}/workload kubectl -n monitoring get ${prometheus} >/dev/null 2>&1"
wl -n monitoring rollout status "${prometheus}" --timeout=15m
# Every component's job, each scraped with its server verified; k3s's components are in its
# kubelet's metrics.
jobs="apiserver kubelet coredns kube-state-metrics node-exporter kube-etcd"
[ "${distribution}" = k3s ] || jobs="${jobs} kube-controller-manager kube-scheduler kube-proxy"
targets='/api/v1/namespaces/monitoring/services/http:kube-prometheus-stack-prometheus:9090/proxy/api/v1/targets?state=active'
all_up() {
    # shellcheck disable=SC2310 # wl wraps one kubectl call; a failed request is only a retry.
    active=$(wl get --raw "${targets}") || return 1
    for job in ${jobs}; do
        printf '%s' "${active}" | jq -e --arg j "${job}" 'any(.data.activeTargets[]; .labels.job == $j)' > /dev/null || return 1
    done
    printf '%s' "${active}" | jq -e 'all(.data.activeTargets[]; .health == "up")' > /dev/null
}
deadline=$(($(date +%s) + 900))
# shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
until all_up; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        # A Service without ready endpoints leaves its job out of the targets altogether, so the
        # missing jobs are named beside the ones down, with the pods that would serve them.
        active=$(wl get --raw "${targets}")
        for job in ${jobs}; do
            printf '%s' "${active}" | jq -e --arg j "${job}" 'any(.data.activeTargets[]; .labels.job == $j)' > /dev/null ||
                echo "no target for ${job}" >&2
        done
        printf '%s' "${active}" | jq -r '.data.activeTargets[] | select(.health != "up") | "\(.labels.job) \(.scrapeUrl): \(.lastError)"' >&2
        wl -n kube-system get daemonsets -o wide >&2
        wl get pods -A -o wide | awk 'NR == 1 || $3 !~ /^([0-9]+)\/\1$/ || $4 != "Running"' >&2
        wl -n kube-system get events --field-selector type=Warning >&2
        fail "monitoring does not reach every component"
    fi
    sleep 10
done
if [ "${distribution}" = k3s ]; then
    # The chart's rules and dashboards find k3s's controller manager, scheduler and kube-proxy in
    # the kubelet's job, so their own series have to arrive there.
    has_series() {
        query=$(printf '%s' "$1" | jq -sRr @uri)
        # shellcheck disable=SC2310 # wl wraps one kubectl call; a failed request is only a retry.
        found=$(wl get --raw "/api/v1/namespaces/monitoring/services/http:kube-prometheus-stack-prometheus:9090/proxy/api/v1/query?query=${query}") || return 1
        printf '%s' "${found}" | jq -e '.data.result | length > 0' > /dev/null
    }
    for series in 'workqueue_adds_total{job="kubelet",name="deployment"}' \
        'scheduler_schedule_attempts_total{job="kubelet"}' \
        'kubeproxy_sync_proxy_rules_duration_seconds_count{job="kubelet"}'; do
        until_true 300 "${series} in Prometheus" has_series "${series}"
    done
fi
pod_security=$(wl label --dry-run=server --overwrite namespace monitoring pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "monitoring's pods do not meet Pod Security's restricted level: ${pod_security}" ;;
    *) ;;
esac

step "logs switched on, collected on every node"
set_variable logs '{"enabled":true,"collector":"node","retentionDays":7,"sizeGiB":10}'
until_true 900 "Loki installed" sh -c "KUBECONFIG=${work}/workload kubectl -n logging get statefulset/loki >/dev/null 2>&1"
wl -n logging rollout status statefulset/loki --timeout=10m
until_true 600 "Alloy installed on the nodes" sh -c "KUBECONFIG=${work}/workload kubectl -n logging-node get daemonset/alloy-node >/dev/null 2>&1"
wl -n logging-node rollout status daemonset/alloy-node --timeout=10m
wl -n logging rollout status deployment/alloy --timeout=10m
# Grafana reads Loki with its client certificate once its Deployment mounts it, which replaces its pod.
grafana_deployment=deployment/kube-prometheus-stack-grafana
grafana_mounts() {
    wl -n monitoring get "${grafana_deployment}" -o json |
        jq -e 'any(.spec.template.spec.volumes[]; .secret.secretName == "kubehenge-loki-client")' > /dev/null
}
until_true 600 "Grafana mounts its Loki client certificate" grafana_mounts
wl -n monitoring rollout status "${grafana_deployment}" --timeout=10m
grafana_user=$(wl -n monitoring get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-user}' | base64 -d)
grafana_password=$(wl -n monitoring get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' | base64 -d)
grafana_port=$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 10000))
KUBECONFIG="${work}/workload" kubectl -n monitoring port-forward "${grafana_deployment}" "${grafana_port}:3000" > "${work}/grafana-forward.log" 2>&1 &
grafana_pid=$!
# grafana <path> [curl options...]: Grafana's API, as its admin.
grafana() {
    grafana_path=$1
    shift
    curl -sS --fail --max-time 30 -u "${grafana_user}:${grafana_password}" "$@" "http://127.0.0.1:${grafana_port}${grafana_path}"
}
until_true 60 "Grafana reached" grafana /api/health
# shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
loki_healthy() { health=$(grafana /api/datasources/uid/loki/health) && printf '%s' "${health}" | jq -e '.status == "OK"' > /dev/null; }
until_true 300 "Grafana reaches Loki with its client certificate" loki_healthy
# lines <LogQL query> <text>: how many of the last hour's lines the query finds in Loki through
# Grafana's Loki data source hold the text.
lines() {
    query=$(jq -cn --arg e "$1" '{from: "now-1h", to: "now", queries: [{refId: "A", datasource: {uid: "loki", type: "loki"}, expr: $e, queryType: "range", maxLines: 1000}]}')
    # shellcheck disable=SC2310 # its failure is returned.
    found=$(grafana /api/ds/query -X POST -H 'Content-Type: application/json' -d "${query}") || return 1
    printf '%s' "${found}" | jq --arg t "$2" '[.results.A.frames[]?.data.values[]? | arrays | .[] | strings | select(contains($t))] | length'
}
# shellcheck disable=SC2310 # polled with set -e off, so a failed query is only a retry.
logged() { count=$(lines "$1" "$2") && [ "${count}" -ge 1 ]; }
# Without a client certificate or a token Loki's proxy lets nobody read, through the API server's
# service proxy no more than from anywhere else.
# shellcheck disable=SC2310 # the refusal is the answer looked for.
wl get --request-timeout=20s --raw /api/v1/namespaces/logging/services/https:loki:3100/proxy/loki/api/v1/labels > /dev/null 2>&1 &&
    fail "Loki answers a client it does not know"
wl create namespace e2e-logs
# log_pod <name> <line> <ports JSON> <then>: a pod at Pod Security's restricted level that writes the
# line, declares the ports and then runs the command.
log_pod() {
    wl -n e2e-logs apply -f - << EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: log
      image: ${agnhost}
      command: [sh, -c, "echo $2; $4"]
      ports: $3
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
EOF
}
# A pod gone again within a second: the node collector reads its log from the node.
short_line=e2e-short-lived-${suffix}
log_pod short "${short_line}" '[]' 'true'
wl -n e2e-logs wait --for=jsonpath='{.status.phase}'=Succeeded pod/short --timeout=5m
until_true 300 "the short-lived pod's line in Loki" logged '{namespace="e2e-logs", pod="short", container="log", job="pods"}' "${short_line}"
until_true 300 "the nodes' journals in Loki" logged '{job="journal"}' ""
# The API server audited the pod's creation, after the collector started reading the log.
until_true 300 "the API server's audit log in Loki" logged '{job="audit"} |= "e2e-logs"' '"name":"short"'
wl -n e2e-logs delete pod/short --wait --timeout=2m
pod_security=$(wl label --dry-run=server --overwrite namespace logging pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "Loki's and the events collector's pods do not meet Pod Security's restricted level: ${pod_security}" ;;
    *) ;;
esac

step "logs collected through the Kubernetes API instead"
set_variable logs '{"enabled":true,"collector":"api","retentionDays":7,"sizeGiB":10}'
until_true 600 "Alloy removed from the nodes" absent wl -n logging-node get daemonset/alloy-node
wl -n logging rollout status deployment/alloy --timeout=10m
# Two ports, which discovery gives the container once each: its line still arrives once.
api_line=e2e-through-the-api-${suffix}
log_pod ported "${api_line}" '[{"containerPort": 8080}, {"containerPort": 8081}]' 'exec /agnhost pause'
wl -n e2e-logs wait --for=condition=Ready pod/ported --timeout=5m
until_true 300 "the pod's line in Loki through the API" logged '{namespace="e2e-logs", pod="ported", job="pods"}' "${api_line}"
# shellcheck disable=SC2310 # a failed query fails the step.
count=$(lines '{namespace="e2e-logs", pod="ported", job="pods"}' "${api_line}") || fail "cannot query Loki"
[ "${count}" = 1 ] || fail "the pod's line arrived ${count} times"
wl delete namespace e2e-logs --wait --timeout=5m

step "logs collected on every node again"
set_variable logs '{"enabled":true,"collector":"node","retentionDays":7,"sizeGiB":10}'
until_true 600 "Alloy installed on the nodes" sh -c "KUBECONFIG=${work}/workload kubectl -n logging-node get daemonset/alloy-node >/dev/null 2>&1"
wl -n logging-node rollout status daemonset/alloy-node --timeout=10m

step "an S3 store for the backups"
target=$(printf '%s' "${class_json}" | jq -c --arg t "${KUBEHENGE_E2E_BACKUP_TARGET}" \
    '.metadata.annotations["kubehenge.dev/backup-targets"] // "[]" | fromjson | .[] | select(.name == $t)')
[ -n "${target}" ] || fail "${class} offers no backup target ${KUBEHENGE_E2E_BACKUP_TARGET}"
s3_endpoint=$(printf '%s' "${target}" | jq -r .endpoint)
bucket=$(printf '%s' "${target}" | jq -r .bucket)
region=$(printf '%s' "${target}" | jq -r '.region // "us-east-1"')
# Where the cluster's backups go in the bucket, as Kubehenge names it.
prefix=$(printf '%s' "${target}" | jq -r --arg own "${ns}.${name}/velero" 'if (.prefix // "") == "" then $own else .prefix + "/" + $own end')
s3_port=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.urlsplit(sys.argv[1]).port or 443)' "${s3_endpoint}")
curl -sS --fail -L -o "${work}/seaweedfs.tar.gz" \
    "https://github.com/seaweedfs/seaweedfs/releases/download/${SEAWEEDFS_VERSION}/linux_amd64.tar.gz"
printf '%s  %s\n' "${SEAWEEDFS_SHA256}" "${work}/seaweedfs.tar.gz" | sha256sum -c - > /dev/null ||
    fail "SeaweedFS's download does not have its SHA-256"
tar -xzf "${work}/seaweedfs.tar.gz" -C "${work}" weed
s3_key=e2e$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')
s3_secret=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
jq -n --arg k "${s3_key}" --arg s "${s3_secret}" \
    '{identities: [{name: "e2e", credentials: [{accessKey: $k, secretKey: $s}], actions: ["Admin", "Read", "Write", "List", "Tagging"]}]}' \
    > "${work}/s3.json"
mkdir -p "${work}/s3-data"
"${work}/weed" server -dir="${work}/s3-data" -ip=127.0.0.1 -ip.bind=0.0.0.0 -master.port=19333 -master.volumeSizeLimitMB=256 \
    -volume.port=18080 -filer.port=18888 -s3 -s3.port="${s3_port}" -s3.config="${work}/s3.json" \
    -s3.cert.file="${KUBEHENGE_E2E_BACKUP_CERT}" -s3.key.file="${KUBEHENGE_E2E_BACKUP_KEY}" > "${work}/s3.log" 2>&1 &
s3_pid=$!
s3() { curl -sS --fail --cacert "${KUBEHENGE_E2E_BACKUP_CA}" --aws-sigv4 "aws:amz:${region}:s3" --user "${s3_key}:${s3_secret}" "$@"; }
until_true 120 "the S3 store takes a bucket" s3 -X PUT -o /dev/null "${s3_endpoint}/${bucket}"

step "backups with Velero: a volume and a Secret backed up, deleted and restored"
marker=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
# As the interface stores a cluster's own key for its target.
kubectl -n "${ns}" create secret generic "${name}.backup" --type=kubehenge.dev/user-credential \
    --from-literal=accessKeyID="${s3_key}" --from-literal=secretAccessKey="${s3_secret}"
kubectl -n "${ns}" label secret "${name}.backup" kubehenge.dev/user-credential=backup "kubehenge.dev/credential-cluster=${name}"
set_variable backups "{\"enabled\":true,\"target\":\"${KUBEHENGE_E2E_BACKUP_TARGET}\"}"
until_true 900 "Velero installed" wl -n velero rollout status deployment/velero --timeout=10s
until_true 600 "Velero's node agent on every node" wl -n velero rollout status daemonset/node-agent --timeout=10s
location_available() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    phase=$(wl -n velero get backupstoragelocation default -o jsonpath='{.status.phase}') && [ "${phase}" = Available ]
}
until_true 300 "Velero reaches the S3 store" location_available
# The node agent reads every pod's volumes from the node, which takes a privileged pod; Velero's
# server needs nothing of the kind.
pod_security=$(wl label --dry-run=server --overwrite namespace velero pod-security.kubernetes.io/enforce=restricted 2>&1)
server_violations=$(printf '%s\n' "${pod_security}" | grep '^Warning: velero-' || true)
[ -z "${server_violations}" ] || fail "Velero's server does not meet Pod Security's restricted level: ${server_violations}"
# The interface's two identities in the cluster: one that reads and starts backups, one that
# restores, neither able to read the keys Velero holds.
# can <Secret> <verb> <resource>: whether the identity whose token the Secret beside the Cluster
# holds may do it in the velero namespace.
can() {
    can_token=$(kubectl -n "${ns}" get secret "$1" -o jsonpath='{.data.token}' | base64 -d)
    server=$(wl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    # shellcheck disable=SC2310 # can-i exits non-zero for its answer no, which the caller reads.
    kubectl --kubeconfig=/dev/null --server="${server}" --certificate-authority="${work}/workload-ca" \
        --token="${can_token}" -n velero auth can-i "$2" "$3" 2> /dev/null || true
}
until_true 300 "the interface's identities in the cluster" sh -c \
    "kubectl -n ${ns} get secret ${name}-backups ${name}-restores > /dev/null 2>&1"
for check in "${name}-backups create backups.velero.io yes" "${name}-backups create restores.velero.io no" \
    "${name}-backups get secrets no" "${name}-restores create restores.velero.io yes" "${name}-restores get secrets no"; do
    # shellcheck disable=SC2086 # the check's words are the arguments.
    set -- ${check}
    answer=$(can "$1" "$2" "$3")
    [ "${answer}" = "$4" ] || fail "$1 may $2 $3: ${answer}, not $4"
done
wl create namespace e2e-backup
wl -n e2e-backup create secret generic e2e --from-literal=marker="${marker}"
wl apply -f - << EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: e2e-backup
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: data
  namespace: e2e-backup
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: data
      image: ${agnhost}
      args: [pause]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
EOF
wl -n e2e-backup wait --for=condition=Ready pod/data --timeout=10m
wl -n e2e-backup exec data -- sh -c "printf '%s' '${marker}' > /data/marker"
# Asked for as the interface's Back up now does: the schedule's own backup, made at once.
template=$(wl -n velero get schedule velero-scheduled -o json | jq -c .spec.template)
excluded=$(printf '%s' "${template}" | jq -r '.excludedNamespaces | index("kube-system")')
[ "${excluded}" != null ] || fail "the schedule backs kube-system up"
jq -n --argjson spec "${template}" '{apiVersion: "velero.io/v1", kind: "Backup", metadata: {name: "e2e", namespace: "velero"}, spec: $spec}' |
    wl apply -f -
# phase_is <kind> <name> <phase>: whether a Velero object has reached the phase; any phase it ends in
# otherwise fails the test.
phase_is() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    phase=$(wl -n velero get "$1" "$2" -o jsonpath='{.status.phase}') || return 1
    case ${phase} in
        "$3") ;;
        Failed | FailedValidation | PartiallyFailed)
            wl -n velero get "$1" "$2" -o yaml >&2
            wl -n velero logs deployment/velero --tail=100 >&2
            fail "$1 $2 ended ${phase}"
            ;;
        *) return 1 ;;
    esac
}
until_true 1200 "the backup completed" phase_is backup e2e Completed
volume_backups=$(wl -n velero get podvolumebackups -l velero.io/backup-name=e2e -o json |
    jq '[.items[] | select(.spec.pod.namespace == "e2e-backup" and .status.phase == "Completed")] | length')
[ "${volume_backups}" = 1 ] || fail "the pod's volume was not backed up: ${volume_backups} volume backups"

step "the backup in the S3 store: under the cluster's own prefix, encrypted"
keys=$(s3 "${s3_endpoint}/${bucket}?list-type=2&max-keys=1000" | grep -o '<Key>[^<]*</Key>' | sed 's/<[^>]*>//g')
stray=$(printf '%s\n' "${keys}" | grep -v -e "^${prefix}/" -e '^$' || true)
[ -z "${stray}" ] || fail "objects outside the cluster's prefix ${prefix}: ${stray}"
printf '%s\n' "${keys}" | grep -q "^${prefix}/kopia/e2e-backup/" || fail "no Kopia repository for e2e-backup under ${prefix}"
tarball="${s3_endpoint}/${bucket}/${prefix}/backups/e2e/e2e.tar.gz"
# Without the cluster's SSE-C key the store gives nothing out; with it, the backup.
plain=$(curl -sS --cacert "${KUBEHENGE_E2E_BACKUP_CA}" --aws-sigv4 "aws:amz:${region}:s3" --user "${s3_key}:${s3_secret}" \
    -o /dev/null -w '%{http_code}' "${tarball}")
[ "${plain}" = 400 ] || fail "the backup was read without its SSE-C key: HTTP ${plain}"
sse_key=$(kubectl -n "${ns}" get secret "${name}-backup-keys" -o jsonpath='{.data.sse-c-key}')
sse_md5=$(printf '%s' "${sse_key}" | base64 -d | openssl dgst -md5 -binary | base64)
s3 -H 'x-amz-server-side-encryption-customer-algorithm: AES256' -H "x-amz-server-side-encryption-customer-key: ${sse_key}" \
    -H "x-amz-server-side-encryption-customer-key-MD5: ${sse_md5}" -o "${work}/e2e.tar.gz" "${tarball}"
contents=$(tar -tzf "${work}/e2e.tar.gz")
printf '%s\n' "${contents}" | grep -q '/namespaces/kube-system/' && fail "the backup holds kube-system"
secret_json=$(tar -xzOf "${work}/e2e.tar.gz" resources/secrets/namespaces/e2e-backup/e2e.json)
backed_up=$(printf '%s' "${secret_json}" | jq -r .data.marker | base64 -d)
[ "${backed_up}" = "${marker}" ] || fail "the backup does not hold the Secret"
# What the store keeps on its disk: the volume's data encrypted by Kopia, the resources by SSE-C.
grep -r -q -F "${marker}" "${work}/s3-data" && fail "the store keeps the volume's data unencrypted"

step "the namespace deleted and restored"
wl delete namespace e2e-backup --wait --timeout=5m
# As the interface restores chosen namespaces.
wl apply -f - << 'EOF'
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: e2e
  namespace: velero
spec:
  backupName: e2e
  includedNamespaces: [e2e-backup]
  restorePVs: true
EOF
until_true 1200 "the restore completed" phase_is restore e2e Completed
wl -n e2e-backup wait --for=condition=Ready pod/data --timeout=10m
restored=$(wl -n e2e-backup exec data -- cat /data/marker)
[ "${restored}" = "${marker}" ] || fail "the volume's data did not come back: ${restored}"
restored=$(wl -n e2e-backup get secret e2e -o jsonpath='{.data.marker}' | base64 -d)
[ "${restored}" = "${marker}" ] || fail "the Secret did not come back"
wl delete namespace e2e-backup --wait --timeout=5m

# The class's minimum sizes have to carry every add-on at once.
step "every add-on at once, with nothing restarted"
found_restarts() {
    if [ "${KUBEHENGE_E2E_RESTARTS:-fail}" = report ]; then
        echo "not failing, on a host that starves its guests: $*"
    else
        fail "$*"
    fi
}
# shellcheck disable=SC2310 # a failed count fails the step.
servers_after=$(server_restarts) || fail "cannot read ${server_unit}'s restarts on the control planes"
[ "${servers_after}" = "${servers_before}" ] ||
    found_restarts "${server_unit} restarted $((servers_after - servers_before)) times on the control planes while the add-ons were installed"
pods_after=$(restarts)
restarted=$(jq -rn --argjson b "${pods_before}" --argjson a "${pods_after}" '$a | to_entries[] | select(.value > ($b[.key] // 0)) | "\(.key) \(.value - ($b[.key] // 0))"')
if [ -n "${restarted}" ]; then
    # Why each container ended, and how starved each node has been of CPU, memory and IO over the
    # last minutes, tell an add-on's fault from nodes too small for them all.
    echo "the restarted containers' last ends:"
    wl get pods -A -o json | jq -r '.items[] | .metadata as $m | .status.containerStatuses[]? |
        select(.restartCount > 0) | .lastState.terminated // {} |
        "\($m.namespace)/\($m.name): \(.reason // "?") exit \(.exitCode // "?") at \(.finishedAt // "?")"'
    for m in $(kubectl -n "${ns}" get machines -o jsonpath='{.items[*].metadata.name}'); do
        echo "pressure on ${m}:"
        # shellcheck disable=SC2310 # a node that cannot be read is said so, not fatal here.
        on_machines "${m}" "grep -H . /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io" ||
            echo "  cannot read it"
    done
    found_restarts "containers restarted while the add-ons were installed (pod, restarts): ${restarted}"
fi
pod_memory "${work}/workload"

step "every add-on's image named by digest"
# Every pod outside kube-system, which only the add-ons, their operators and these tests create,
# and in kube-system the workloads of the releases Kubehenge installs.
releases=$(kubectl -n "${ns}" get helmreleaseproxies -l "cluster.x-k8s.io/cluster-name=${name}" -o json | jq -c '[.items[].spec.releaseName]')
images=$(wl get pods -A -o json | jq -r '.items[] | select(.metadata.namespace != "kube-system") | .metadata as $m |
    (.spec.initContainers // [])[], .spec.containers[] | "\($m.namespace)/\($m.name) \(.image)"')
system_images=$(wl -n kube-system get deployments,daemonsets,statefulsets -o json | jq -r --argjson r "${releases}" '.items[] |
    select(.metadata.annotations["meta.helm.sh/release-name"] as $n | $r | index($n)) | .metadata as $m |
    (.spec.template.spec.initContainers // [])[], .spec.template.spec.containers[] | "kube-system/\($m.name) \(.image)"')
unpinned=$(printf '%s\n%s\n' "${images}" "${system_images}" | grep -v -e '@sha256:[0-9a-f]\{64\}$' -e '^$' | sort -u)
[ -z "${unpinned}" ] || fail "images named without a digest (pod, image): ${unpinned}"

step "every add-on switched off at once"
set_variable headlamp '{"enabled":false}'
# Logs go first: they need monitoring, which is refused off while they are on.
# shellcheck disable=SC2310 # the refusal is the answer looked for.
set_variable monitoring '{"enabled":false}' > /dev/null 2>&1 && fail "monitoring was switched off under logs"
kill "${grafana_pid}" 2> /dev/null || true
grafana_pid=
set_variable logs '{"enabled":false,"collector":"node","retentionDays":7,"sizeGiB":10}'
set_variable monitoring '{"enabled":false}'
set_variable podSecurity '{"admission":"restricted","kyverno":"off"}'
set_variable istio '{"enabled":false}'
set_variable backups "{\"enabled\":false,\"target\":\"${KUBEHENGE_E2E_BACKUP_TARGET}\"}"
until_true 600 "Headlamp removed" absent wl -n headlamp get deployment/headlamp
until_true 600 "Loki removed" absent wl -n logging get statefulset/loki
until_true 300 "Alloy removed" absent wl -n logging get deployment/alloy
until_true 300 "Alloy removed from the nodes" absent wl -n logging-node get daemonset/alloy-node
# shellcheck disable=SC2310 # a volume that cannot be read fails the step.
wl -n logging get pvc/loki-storage > /dev/null || fail "Loki's volume went with logs"
until_true 900 "monitoring removed" absent wl -n monitoring get "${prometheus}"
until_true 300 "metrics proxies removed" absent wl -n kube-system get daemonset/kubehenge-etcd-metrics
until_true 900 "Kyverno removed" absent wl -n kyverno get deployment/kyverno-admission-controller
# shellcheck disable=SC2310 # the admission is the answer looked for.
privileged_pod e2e-privileged > /dev/null || fail "Kyverno's policies outlive Kyverno"
wl delete namespace e2e-privileged --wait --timeout=5m
until_true 900 "Istio removed" absent wl -n istio-node get daemonset/ztunnel
until_true 300 "istiod removed" absent wl -n istio-system get deployment/istiod
enrolled=$(wl get namespace e2e-mesh -o jsonpath='{.metadata.labels.istio\.io/dataplane-mode}')
[ -z "${enrolled}" ] || fail "e2e-mesh is still enrolled in a mesh that has gone"
# Taken out of the mesh first, its pods reach each other and the rest without ztunnel.
until_true 120 "a client outside the old mesh reaches the server" fetches e2e-plain
until_true 60 "a client in the old mesh reaches the server" fetches e2e-mesh
wl delete namespace e2e-mesh e2e-plain --wait --timeout=5m
until_true 900 "Velero removed" absent wl get namespace velero
crds=$(wl get customresourcedefinitions -o name)
velero_crds=$(printf '%s\n' "${crds}" | grep '\.velero\.io$' || true)
[ -z "${velero_crds}" ] || fail "Velero's resource definitions outlive it: ${velero_crds}"
until_true 300 "the interface's identities in the cluster removed" absent kubectl -n "${ns}" get secret "${name}-backups"
# shellcheck disable=SC2310 # absent's answer is what is checked.
absent kubectl -n "${ns}" get secret "${name}-restores" || fail "the restores identity outlives backups"
# The keys stay, so that the backups, which stay in the store, can be read again.
kubectl -n "${ns}" get secret "${name}-backup-keys" > /dev/null || fail "the backup keys went with Velero"
# shellcheck disable=SC2310 # a failed read fails the test with the reason.
s3 -o /dev/null "${tarball}" -H 'x-amz-server-side-encryption-customer-algorithm: AES256' \
    -H "x-amz-server-side-encryption-customer-key: ${sse_key}" -H "x-amz-server-side-encryption-customer-key-MD5: ${sse_md5}" ||
    fail "the backup went from the store with Velero"
kill "${s3_pid}"
s3_pid=

step "the load balancer switched off"
servers=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
set_variable loadBalancer "{\"enabled\":false,\"implementation\":\"${load_balancer}\",\"addresses\":${load_balancer_addresses}}"
case ${load_balancer} in
    kube-vip)
        until_true 600 "kube-vip's Service announcer removed" absent wl -n kube-system get daemonset/kube-vip
        until_true 300 "kube-vip's cloud provider removed" absent wl -n kube-system get deployment/kube-vip-cloud-provider
        ;;
    *) until_true 600 "MetalLB removed" absent wl -n metallb-system get daemonset/metallb-speaker ;;
esac
if [ "${distribution}" = k3s ]; then
    # Switching servicelb back on replaces the control planes, whose servicelb then answers for the
    # Service on every node's own address.
    replaced() {
        kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o json |
            jq -e --arg old " ${servers} " '(.items | length > 0) and all(.items[]; .metadata.name as $name |
                .status.phase == "Running" and (($old | contains(" " + $name + " ")) | not))' > /dev/null
    }
    until_true 1800 "the control planes replaced" replaced
    kubectl -n "${ns}" wait --for=condition=Available "cluster/${name}" --timeout=20m
    on_node() {
        # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
        lb_ips || return 1
        node_addresses=$(wl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}')
        case " ${node_addresses} " in
            *" ${lb_ip%% *} "*) answers "${lb_ip%% *}" ;;
            *) return 1 ;;
        esac
    }
    until_true 600 "servicelb answers for the Service on a node's address" on_node
fi
wl delete namespace e2e-lb --wait --timeout=5m

workers() {
    kubectl -n "${ns}" patch "cluster/${name}" --type=json \
        -p "[{\"op\":\"replace\",\"path\":\"/spec/topology/workers/machineDeployments/0/replicas\",\"value\":$1}]"
    until_true 1800 "$1 worker nodes ready" sh -c "[ \"\$(KUBECONFIG=${work}/workload kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o json | jq '[.items[] | select(any(.status.conditions[]; .type == \"Ready\" and .status == \"True\"))] | length')\" = $1 ] && [ \"\$(kubectl -n ${ns} get machines -l cluster.x-k8s.io/deployment-name -o name | wc -l)\" = $1 ]"
}
step "scale workers to 2"
workers 2
step "scale workers back to 1"
workers 1

step "upgrade to ${upgrade}"
old_servers=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
kubectl -n "${ns}" patch "cluster/${name}" --type=merge -p "{\"spec\":{\"topology\":{\"version\":\"${upgrade}\"}}}"
case ${class} in
    kubehenge-rke2 | kubehenge-k3s)
        # The first new server cannot reach the cluster's API at first, as when the old server is
        # down while it joins, so its service's first start fails. The service keeps retrying and the
        # roll still finishes. k3s's bootstrap then leaves cloud-init in error, which the class does
        # not fail the machine on; RKE2's carries on past the failed start and ends cloud-init done.
        # Kept from the old servers' side, the block is in place before the new one boots; RKE2's
        # servers join through 9345, k3s's through 6443.
        case ${class} in
            kubehenge-rke2) service=rke2-server expected='done' ;;
            *) service=k3s expected=error ;;
        esac
        joining='' joining_address='' joining_vmid='' joining_node='' state=''
        found_joining() {
            joining=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o json |
                jq -r --arg old " ${old_servers} " '[.items[] | .metadata.name as $n | select($old | contains(" " + $n + " ") | not)][0].spec.infrastructureRef.name // empty')
            [ -n "${joining}" ]
        }
        found_address() {
            joining_address=$(kubectl -n "${ns}" get ipaddresses.ipam.cluster.x-k8s.io -o json |
                jq -r --arg m "${joining}" '[.items[] | select(.spec.claimRef.name | startswith($m))][0].spec.address // empty')
            [ -n "${joining_address}" ]
        }
        found_vm() {
            joining_vmid=$(kubectl -n "${ns}" get proxmoxmachine "${joining}" -o jsonpath='{.spec.virtualMachineID}')
            joining_node=$(kubectl -n "${ns}" get proxmoxmachine "${joining}" -o jsonpath='{.status.proxmoxNode}')
            [ -n "${joining_vmid}" ] && [ -n "${joining_node}" ]
        }
        cloud_init_ended() {
            # shellcheck disable=SC2310 # polled until cloud-init has ended; a failed attempt is tried again.
            state=$(guest "${joining_node}" "${joining_vmid}" 'cloud-init status 2>&1 || true' 2> /dev/null | sed -n 's/^status: //p') || return 1
            [ -n "${state}" ] && [ "${state}" != running ]
        }
        until_true 900 "a new control-plane machine" found_joining
        until_true 900 "${joining}'s address" found_address
        on_machines "${old_servers}" "nft add table inet e2e-join && nft add chain inet e2e-join in '{ type filter hook input priority 0; }' && nft add rule inet e2e-join in ip saddr ${joining_address} tcp dport '{ 6443, 9345 }' reject with tcp reset"
        printf 'the old servers refuse %s (%s)\n' "${joining_address}" "${joining}"
        until_true 900 "${joining}'s VM" found_vm
        until_true 1200 "cloud-init finished on ${joining}" cloud_init_ended
        [ "${state}" = "${expected}" ] || fail "cloud-init on ${joining} ended ${state}, not ${expected}, with the cluster's API refusing it"
        # shellcheck disable=SC2310 # guest returns the command's status, which is the one tested.
        guest "${joining_node}" "${joining_vmid}" "journalctl -u ${service}.service --no-pager | grep -q 'Failed to start'" > /dev/null ||
            fail "${service} on ${joining} started although the cluster's API refused it"
        on_machines "${old_servers}" 'nft delete table inet e2e-join'
        printf '%s on %s failed to start and cloud-init ended %s; the old servers accept it again\n' "${service}" "${joining}" "${state}"
        ;;
    # A failed kubeadm join is not retried, so kubeadm's upgrade runs without the refused join.
    *) ;;
esac
kubelet=${upgrade%%+*}
until_true 3600 "every node on ${kubelet}" sh -c "KUBECONFIG=${work}/workload kubectl get nodes -o json | jq -e --arg v '${kubelet}' '(.items | length == 2) and (.items | all(.status.nodeInfo.kubeletVersion | startswith(\$v))) and (.items | all(any(.status.conditions[]; .type == \"Ready\" and .status == \"True\")))' >/dev/null"
kubectl -n "${ns}" wait --for=condition=Available "cluster/${name}" --timeout=20m
wl get nodes -o wide
# The new control-plane nodes have to carry the authentication configuration too.
check_sign_in
# The distribution's own Gateway API chart takes over what its Traefik CRD chart shipped before,
# and Kubehenge's policy, without Kubehenge applying anything beside it.
if [ -n "${gateway_api_chart}" ]; then
    until_true 300 "${gateway_api_chart} holds the Gateway API after the upgrade" gateway_api_chart_holds
fi
until_true 300 "the Gateway API's objects after the upgrade, each held once" gateway_api_held
until_true 300 "Traefik's Gateway is programmed after the upgrade" gateway_programmed

step "a dead worker is replaced"
# The class's 300 s before a not-ready node counts as dead only adds waiting here; the integration
# tier pins the classes' own checks. The override has to repeat the class's remediation, or it would
# drop it.
kubectl -n "${ns}" patch "cluster/${name}" --type=json -p '[{"op": "add", "path": "/spec/topology/workers/machineDeployments/0/healthCheck", "value": {
  "checks": {"nodeStartupTimeoutSeconds": 1200, "unhealthyNodeConditions": [
    {"type": "Ready", "status": "Unknown", "timeoutSeconds": 60}, {"type": "Ready", "status": "False", "timeoutSeconds": 60}]},
  "remediation": {"maxInFlight": 1}}}]'
until_true 120 "the workers' health check at 60 s" sh -c "kubectl -n ${ns} get machinehealthchecks -o json |
    jq -e '[.items[] | select(.spec.selector.matchLabels[\"topology.cluster.x-k8s.io/deployment-name\"] == \"md-0\")
    | .spec.checks.unhealthyNodeConditions[].timeoutSeconds] | length > 0 and all(. == 60)' >/dev/null"
machine=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/deployment-name -o jsonpath='{.items[0].metadata.name}')
infra=$(kubectl -n "${ns}" get machine "${machine}" -o jsonpath='{.spec.infrastructureRef.name}')
vmid=$(kubectl -n "${ns}" get proxmoxmachine "${infra}" -o jsonpath='{.spec.virtualMachineID}')
node=$(kubectl -n "${ns}" get proxmoxmachine "${infra}" -o jsonpath='{.status.proxmoxNode}')
printf 'stopping VM %s (%s) on %s\n' "${vmid}" "${machine}" "${node}"
pve -X POST "${KUBEHENGE_PROXMOX_URL}/api2/json/nodes/${node}/qemu/${vmid}/status/stop" > /dev/null
until_true 1800 "machine ${machine} replaced" absent kubectl -n "${ns}" get machine "${machine}"
workers 1

step "delete"
kubectl -n "${ns}" delete "cluster/${name}" --wait --timeout=30m
# A failed request is not an empty list.
# shellcheck disable=SC2310 # polled; a failed request leaves the pool counted as there.
pool_removed() { pools=$(pve_get pools) && ! printf '%s' "${pools}" | jq -e --arg p "${pool}" '.data[] | select(.poolid == $p)' > /dev/null; }
until_true 300 "pool ${pool} removed" pool_removed
users=$(pve_get access/users)
printf '%s' "${users}" | jq -e --arg u "${user}" '[.data[] | select(.userid == $u)] | length == 0' > /dev/null || fail "user ${user} is still there"
vms=$(pve_get "cluster/resources?type=vm")
printf '%s' "${vms}" | jq -e --argjson o "${owner}" --arg n "${name}" '[.data[] | select(.vmid == $o or (.name // "" | startswith($n + "-")))] | length == 0' > /dev/null ||
    fail "VMs of the cluster are still there"
if ! kubectl delete namespace "${ns}" --wait --timeout=5m; then
    # Its conditions say what is left in it and whose finalizers hold that.
    left=$(kubectl get namespace "${ns}" -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.message}; {end}' || true)
    fail "namespace ${ns} is still there: ${left}"
fi

# Another test may be using the class on the same management cluster.
if kubectl get clusters --all-namespaces -o json | jq -e --arg c "${class}" '[.items[] | select(.spec.topology.classRef.name == $c)] | length == 0' > /dev/null; then
    step "${class}'s providers stop with its last cluster"
    until_true 300 "${class}'s providers idle" providers_idle "${class}"
fi

printf '\nPASS\n'
