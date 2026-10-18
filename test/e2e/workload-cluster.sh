#!/bin/sh
# End-to-end test of one Kubehenge ClusterClass on a Proxmox VE host, against a running management
# cluster: create a cluster trusting an OpenID issuer and enforcing Pod Security's restricted level,
# check sign-in through it, its nodes, cloud controller manager and persistent volumes, switch
# Headlamp, monitoring (every component scraped with its server verified) and Kyverno on and off,
# scale its workers up and down, upgrade it by one minor version (for RKE2 and k3s with the first
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
#   KUBEHENGE_E2E_ENDPOINT (the control plane's virtual IP), KUBEHENGE_E2E_STORAGE.
# KUBEHENGE_E2E_CONTROL_PLANE_MEMORY and KUBEHENGE_E2E_WORKER_MEMORY (MiB) and
# KUBEHENGE_E2E_CONTROL_PLANE_CORES size the nodes.
# KUBEHENGE_E2E_CNI chooses the CNI (required by kubeadm). KUBEHENGE_E2E_PODS and
# KUBEHENGE_E2E_SERVICES set the cluster's pod and service ranges, which must not overlap the nodes'
# network; unset, the distribution's defaults apply.
# KUBEHENGE_E2E_OIDC_ADDRESS is an IPv4 address of the machine running this test that the cluster's
# nodes reach; the test serves an OpenID issuer there, on KUBEHENGE_E2E_OIDC_PORT (default 8943),
# which the cluster trusts. Needs openssl and python3.
set -eu

class=${1:?usage: $0 <ClusterClass> <version> <upgrade version>}
version=${2:?usage: $0 <ClusterClass> <version> <upgrade version>}
upgrade=${3:?usage: $0 <ClusterClass> <version> <upgrade version>}
: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_ADDRESSES:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}"
: "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_ENDPOINT:?}" "${KUBEHENGE_E2E_STORAGE:?}" "${KUBEHENGE_E2E_OIDC_ADDRESS:?}"

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

work=$(mktemp -d)
distribution=$(echo "${class}" | sed 's/^kubehenge-//')
suffix=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
name=e2e-${distribution}-${suffix}
ns=${name}
issuer_pid=
cleanup() {
    status=$?
    [ -z "${issuer_pid}" ] || kill "${issuer_pid}" 2> /dev/null || true
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
cni_variable=
if [ -n "${KUBEHENGE_E2E_CNI:-}" ]; then
    cni_variable=$(printf '      - name: cni\n        value: %s\n' "${KUBEHENGE_E2E_CNI}")
fi
oidc=$(jq -cn --arg i "${issuer}" --rawfile ca "${work}/ca.crt" '[{issuerURL: $i, clientID: "kubehenge-e2e", certificateAuthority: $ca}]')
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
      - name: controlPlaneMachine
        value: {memoryMiB: ${KUBEHENGE_E2E_CONTROL_PLANE_MEMORY:-4096}, cores: ${KUBEHENGE_E2E_CONTROL_PLANE_CORES:-2}}
      - name: workerMachine
        value: {memoryMiB: ${KUBEHENGE_E2E_WORKER_MEMORY:-4096}}
${cni_variable}
      - name: oidc
        value: ${oidc}
      # The strictest level from the start, so that every component Kubehenge installs is seen to
      # run under it, or to sit in a namespace it exempts.
      - name: podSecurity
        value: {admission: restricted}
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
class_json=$(kubectl -n kubehenge-system get clusterclass "${class}" -o json)
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
agnhost=registry.k8s.io/e2e-test-images/agnhost:2.61
# agnhost_pod <name> <args...>: an agnhost pod at Pod Security's restricted level, which the
# cluster enforces; the image runs as root unless told otherwise. The overrides merge into the
# container by its name. sh has no local variables, so its own carry its prefix.
agnhost_pod() {
    agnhost_overrides=$(printf '{"apiVersion":"v1","spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"%s","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' "$1")
    agnhost_name=$1
    shift
    wl -n e2e-policy run "${agnhost_name}" --image="${agnhost}" --override-type=strategic --overrides="${agnhost_overrides}" -- "$@"
}
wl create namespace e2e-policy
agnhost_pod server netexec --http-port=8080
agnhost_pod client pause
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

step "a persistent volume from the Proxmox VE CSI plugin"
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
      image: registry.k8s.io/pause:3.10.1
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
owned() {
    first_node=$(pve_get nodes | jq -r '.data[0].node')
    pve_get "nodes/${first_node}/storage/${KUBEHENGE_E2E_STORAGE}/content?content=images&vmid=${owner}" | jq '.data | length'
}
owned_volumes=$(owned)
[ "${owned_volumes}" -ge 1 ] || fail "no volume owned by the volume owner VM ${owner}"
wl -n default delete pod/e2e pvc/e2e --wait --timeout=5m
# The CSI plugin retries a failed deletion with a back-off of up to five minutes.
until_true 600 "volume deleted" sh -c "[ \"\$(KUBECONFIG=${work}/workload kubectl get pv -o name | wc -l)\" = 0 ]"
owned_volumes=$(owned)
[ "${owned_volumes}" = 0 ] || fail "the volume is still on ${KUBEHENGE_E2E_STORAGE}"

# set_variable <name> <JSON value>: sets one of the Cluster's topology variables.
set_variable() {
    vars=$(kubectl -n "${ns}" get "cluster/${name}" -o json |
        jq -c --arg n "$1" --argjson v "$2" '[.spec.topology.variables[] | select(.name != $n)] + [{name: $n, value: $v}]')
    kubectl -n "${ns}" patch "cluster/${name}" --type=merge -p "{\"spec\":{\"topology\":{\"variables\":${vars}}}}"
}
step "Headlamp switched on and off"
set_variable headlamp '{"enabled":true}'
until_true 600 "Headlamp installed" sh -c "KUBECONFIG=${work}/workload kubectl -n headlamp get deployment/headlamp >/dev/null 2>&1"
wl -n headlamp rollout status deployment/headlamp --timeout=10m
# Through the API server's service proxy, as Kubehenge's interface reaches it.
page=$(wl get --raw /api/v1/namespaces/headlamp/services/http:headlamp:80/proxy/)
printf '%s' "${page}" | grep -q -i '<html' || fail "Headlamp does not answer through the API server"
# shellcheck disable=SC2310 # can-i exits non-zero for its answer no, which the comparison reads.
can_list=$(wl auth can-i list pods --all-namespaces --as=system:serviceaccount:headlamp:headlamp || true)
[ "${can_list}" = no ] || fail "Headlamp's service account has rights in the cluster"
pod_security=$(wl label --dry-run=server --overwrite namespace headlamp pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "Headlamp's pod does not meet Pod Security's restricted level" ;;
    # Only a pod that falls short of the level gets a warning.
    *) ;;
esac
set_variable headlamp '{"enabled":false}'
until_true 600 "Headlamp removed" absent wl -n headlamp get deployment/headlamp

step "monitoring switched on and off"
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
        wl get --raw "${targets}" | jq -r '.data.activeTargets[] | select(.health != "up") | "\(.labels.job) \(.scrapeUrl): \(.lastError)"' >&2
        fail "monitoring does not reach every component"
    fi
    sleep 10
done
pod_security=$(wl label --dry-run=server --overwrite namespace monitoring pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "monitoring's pods do not meet Pod Security's restricted level: ${pod_security}" ;;
    *) ;;
esac
set_variable monitoring '{"enabled":false}'
until_true 900 "monitoring removed" absent wl -n monitoring get "${prometheus}"
until_true 300 "metrics proxies removed" absent wl -n kube-system get daemonset/kubehenge-etcd-metrics

step "pod security enforced, with Kyverno switched on and off"
# privileged_pod <namespace>: asks the API server to admit a privileged pod, without creating it.
privileged_pod() {
    wl -n "$1" run e2e-privileged --image=registry.k8s.io/pause:3.10.1 --restart=Never --dry-run=server \
        --overrides='{"apiVersion":"v1","spec":{"containers":[{"name":"e2e-privileged","image":"registry.k8s.io/pause:3.10.1","securityContext":{"privileged":true}}]}}' 2>&1
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
set_variable podSecurity '{"admission":"restricted","kyverno":"off"}'
until_true 900 "Kyverno removed" absent wl -n kyverno get deployment/kyverno-admission-controller
# shellcheck disable=SC2310 # the admission is the answer looked for.
privileged_pod e2e-privileged > /dev/null || fail "Kyverno's policies outlive Kyverno"
wl delete namespace e2e-privileged --wait --timeout=5m

workers() {
    kubectl -n "${ns}" patch "cluster/${name}" --type=json \
        -p "[{\"op\":\"replace\",\"path\":\"/spec/topology/workers/machineDeployments/0/replicas\",\"value\":$1}]"
    until_true 1800 "$1 worker nodes ready" sh -c "[ \"\$(KUBECONFIG=${work}/workload kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o json | jq '[.items[] | select(any(.status.conditions[]; .type == \"Ready\" and .status == \"True\"))] | length')\" = $1 ] && [ \"\$(kubectl -n ${ns} get machines -l cluster.x-k8s.io/deployment-name -o name | wc -l)\" = $1 ]"
}
step "scale workers to 2"
workers 2
step "scale workers back to 1"
workers 1

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
kubectl delete namespace "${ns}" --wait --timeout=5m

# Another test may be using the class on the same management cluster.
if kubectl get clusters --all-namespaces -o json | jq -e --arg c "${class}" '[.items[] | select(.spec.topology.classRef.name == $c)] | length == 0' > /dev/null; then
    step "${class}'s providers stop with its last cluster"
    until_true 300 "${class}'s providers idle" providers_idle "${class}"
fi

printf '\nPASS\n'
