#!/bin/sh
# End-to-end test of one Kubehenge ClusterClass on a Proxmox VE host, against a running management
# cluster: create a cluster trusting an OpenID issuer, hardened to the CIS benchmark and enforcing
# Pod Security's restricted level, check sign-in through it, its nodes against the benchmark, its
# cloud controller manager, ingress controller, Gateway API, load balancer and persistent volumes,
# what the health page's read-only identities may see in it, that the shared Headlamp's sign-in
# identities are there for the interface's namespaces alone and do no more than they should, switch
# Headlamp (and its read-only access for everyone), Kyverno, Istio (a namespace's traffic over mutual TLS), monitoring (every
# component scraped with its server verified) and logs (a pod's lines, and with the node collector
# the journal and the audit log, read in Loki through Grafana) on and off, back a namespace with a
# volume and a Secret up with Velero to an S3 store the test serves, delete it and restore it, check
# the store holds the backup encrypted, switch certificates on (from the cluster's own CA, then from
# an ACME server over DNS-01 and over HTTP-01) and GitOps (Flux, then Argo CD, syncing a repository
# on a Git server in the cluster through a deploy key), on Talos upgrade the nodes in place to a
# newer Talos release with every add-on on, switch backups, certificates and GitOps off, take an
# etcd snapshot into the same store, read it back and restore etcd from it through the guest agent
# (on Talos through the Talos API), on RKE2 and k3s have the servers upload a snapshot of their
# own there and switch that off, switch the load balancer off, scale its workers up and down,
# upgrade it by one minor version (for RKE2 and k3s with the first new server's join refused at
# first), replace a node that dies, delete it, and check that nothing of it is left on Proxmox VE.
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
# KUBEHENGE_E2E_CONTROL_PLANE_MEMORY and KUBEHENGE_E2E_WORKER_MEMORY (MiB),
# KUBEHENGE_E2E_CONTROL_PLANE_CORES and KUBEHENGE_E2E_WORKER_CORES size the nodes; the memory, and
# the worker's cores, are by default the least the class lets each have with the add-ons.
# KUBEHENGE_E2E_RESTARTS=report prints restarts while every
# add-on is on instead of failing, for a host whose KVM lacks pause-loop exiting: there guests that
# outnumber its CPUs starve each other, whatever their sizes.
# KUBEHENGE_E2E_NODE_OS is the nodes' operating system as one word, such as rocky-10, whose
# template has to exist; unset, the class's default applies. Talos nodes run Talos and take none.
# KUBEHENGE_E2E_CIS=false creates the cluster without CIS hardening, the class's default, and leaves
# the benchmark out; by default the cluster is hardened and its nodes checked against it.
# KUBEHENGE_E2E_CNI chooses the CNI (required by kubeadm). KUBEHENGE_E2E_PODS and
# KUBEHENGE_E2E_SERVICES set the cluster's pod and service ranges, which must not overlap the nodes'
# network; unset, the distribution's defaults apply.
# KUBEHENGE_E2E_OIDC_ADDRESS is an IPv4 address of the machine running this test that the cluster's
# nodes reach; the test serves an OpenID issuer there, on KUBEHENGE_E2E_OIDC_PORT (default 8943),
# which the cluster trusts. Needs openssl, python3 and ssh-keygen.
# KUBEHENGE_E2E_DATASTORE_ALLOCATE=1 switches the CSI plugin's newest release on and off again, on a
# KUBEHENGE_E2E_STORAGE that the management cluster, the templates and other clusters leave alone,
# with a management cluster that lets clusters choose it; KUBEHENGE_E2E_LOCAL_STORAGE names one of
# the management cluster's storages, which a cluster asking for it is refused.
# KUBEHENGE_E2E_BACKUP_TARGET names a backup target the management cluster was set up with whose
# endpoint is on this machine, at an https:// address the cluster's nodes reach; the test serves
# SeaweedFS's S3 gateway there with KUBEHENGE_E2E_BACKUP_CERT and KUBEHENGE_E2E_BACKUP_KEY, which
# KUBEHENGE_E2E_BACKUP_CA, the target's CA, signed. The etcd snapshots go to the same target; the
# management cluster restores etcd through the guest agent (etcd_restore; Talos clusters through
# their Talos API either way), and Go builds the
# kubehenge command that decrypts a snapshot.
# KUBEHENGE_E2E_HA_GROUPS=1 checks the cloud controller manager's HA group labels, which a cluster
# has by default where the management cluster leaves them on, then switches them off and on again;
# the test makes an HA node-affinity rule over every node, which needs Proxmox VE 9, and removes it
# again.
# KUBEHENGE_E2E_NAMESPACE names one of the namespaces the management cluster's
# extra_cluster_namespaces gives the web interface: the cluster is then created there with the
# interface's own account, as the interface creates one in a namespace of its creator's choice, and
# the namespace is left in place; with the management cluster's headlamp_sign_in, the cluster gets
# the shared Headlamp's sign-in identities there. Unset, the cluster gets a namespace of its own,
# created and deleted with it, and none of those identities.
# KUBEHENGE_E2E_VOLUME_SNAPSHOTS=1 switches volume snapshots on, takes a snapshot of a volume with
# data, restores it into a new one and checks the data, and switches them off again, with a
# management cluster that lets clusters take them, root@pam's password stored, and a
# KUBEHENGE_E2E_STORAGE local to each node.
# KUBEHENGE_E2E_TALOS_VERSION and KUBEHENGE_E2E_TALOS_UPGRADE are, for kubehenge-talos, the Talos
# release the cluster starts on and a newer one its nodes are upgraded to in place, both with a
# template in the template pool. Talos has no SSH, no systemd and no guest agent that runs commands:
# the steps that read a node from inside read it through Talos's API, with talosctl and the
# cluster's talosconfig, or are left out.
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
SEAWEEDFS_VERSION=4.51
SEAWEEDFS_SHA256=c03be37416a8ee672c77766e20b22a4e1430744c3af2668599a4b6eeaec14cee
# etcdutl, which reads the etcd snapshots back.
# renovate: datasource=github-release-attachments depName=etcd-io/etcd
ETCD_VERSION=v3.7.3
ETCD_SHA256=5dfa9cc3b3ce3b4b491fbcc2057d3daa166bcc6676f743fcd20f8e5760cb9775

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"
# shellcheck source=cis-benchmark.sh
. "$(dirname "$0")/cis-benchmark.sh"

cis=${KUBEHENGE_E2E_CIS:-true}
case ${cis} in
    true | false) ;;
    *) fail "KUBEHENGE_E2E_CIS is true or false, not ${cis}" ;;
esac

work=$(mktemp -d)
distribution=$(echo "${class}" | sed 's/^kubehenge-//')
suffix=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
name=e2e-${distribution}-${suffix}
ns=${KUBEHENGE_E2E_NAMESPACE:-${name}}
issuer_pid=
grafana_pid=
s3_pid=
recorder_pid=
# The HA rule the nodes are labelled with, and the guest it needs.
ha_rule=''
ha_vmid=''
ha_node=''
# calico_state: what the Tigera operator says of the cluster's Calico, which no collected log keeps
# while the operator runs without restarts.
calico_state() {
    [ "${KUBEHENGE_E2E_CNI:-}" = calico ] || return 0
    kubectl -n "${ns}" get secret "${name}-kubeconfig" -o jsonpath='{.data.value}' 2> /dev/null | base64 -d > "${work}/state" || return 0
    printf '\nCalico:\n'
    KUBECONFIG=${work}/state kubectl get tigerastatus -o wide 2>&1 || true
    KUBECONFIG=${work}/state kubectl -n tigera-operator logs deployment/tigera-operator --tail=40 2>&1 || true
}
cleanup() {
    status=$?
    [ "${status}" -eq 0 ] || calico_state
    [ -z "${issuer_pid}" ] || kill "${issuer_pid}" 2> /dev/null || true
    [ -z "${grafana_pid}" ] || kill "${grafana_pid}" 2> /dev/null || true
    [ -z "${s3_pid}" ] || kill "${s3_pid}" 2> /dev/null || true
    [ -z "${recorder_pid}" ] || kill "${recorder_pid}" 2> /dev/null || true
    # shellcheck disable=SC2310 # each removal is tried, whatever happened to those before it
    remove_ha_rule || true
    if [ "${status}" -ne 0 ] && [ -n "${KUBEHENGE_E2E_NAMESPACE:-}" ]; then
        printf '\nleaving cluster %s/%s for inspection; delete it with: kubectl -n %s delete cluster %s\n' "${ns}" "${name}" "${ns}" "${name}"
    elif [ "${status}" -ne 0 ]; then
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
    # Requests without credentials reach the health checks on kubeadm and Talos, whose join and API
    # server probes need them, and nothing else; on RKE2 and k3s, nothing at all.
    server=$(wl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    for path in /readyz /version; do
        code=$(curl -sS --cacert "${work}/workload-ca" -o /dev/null -w '%{http_code}' "${server}${path}")
        case ${distribution}${path} in
            kubeadm/readyz | talos/readyz) want=200 ;;
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
# creator: kubectl as whoever creates the cluster, the interface's account in one of its namespaces.
if [ -n "${KUBEHENGE_E2E_NAMESPACE:-}" ]; then
    kubectl -n kubehenge-system get configmap kubehenge-interface -o jsonpath='{.data.namespaces}' |
        jq -e --arg n "${ns}" 'index($n) > 0' > /dev/null || fail "the interface is not given namespace ${ns}"
    kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "${work}/management-ca.crt"
    management_server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    interface_token=$(kubectl -n kubehenge-system create token kubehenge-interface --duration=2h)
    creator_config="${work}/interface.kubeconfig"
    kubectl --kubeconfig "${creator_config}" config set-cluster management --server="${management_server}" \
        --certificate-authority="${work}/management-ca.crt" --embed-certs > /dev/null
    kubectl --kubeconfig "${creator_config}" config set-credentials interface --token="${interface_token}" > /dev/null
    kubectl --kubeconfig "${creator_config}" config set-context interface --cluster=management --user=interface > /dev/null
    kubectl --kubeconfig "${creator_config}" config use-context interface > /dev/null
    creator() { kubectl --kubeconfig "${creator_config}" "$@"; }
    # can-i answers no with a non-zero exit as well as on the word.
    in_namespace=$(kubectl --kubeconfig "${creator_config}" auth can-i create clusters.cluster.x-k8s.io -n "${ns}" || true)
    in_default=$(kubectl --kubeconfig "${creator_config}" auth can-i create clusters.cluster.x-k8s.io -n default || true)
    [ "${in_namespace}" = yes ] || fail "the interface may not create clusters in ${ns}"
    [ "${in_default}" = no ] || fail "the interface may create clusters in default"
else
    kubectl create namespace "${ns}"
    creator() { kubectl "$@"; }
fi
network_spec=$(cluster_network)
addresses=$(json_list "${KUBEHENGE_E2E_ADDRESSES}")
dns_servers=$(json_list "${KUBEHENGE_E2E_DNS}")
load_balancer_addresses=$(json_list "${KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES}")
cni_variable=
if [ -n "${KUBEHENGE_E2E_CNI:-}" ]; then
    cni_variable=$(printf '      - name: cni\n        value: %s\n' "${KUBEHENGE_E2E_CNI}")
fi
talos_variable=
node_os=
if [ "${distribution}" = talos ]; then
    # Talos is its own OS: its class has no nodeOS variable.
    [ -z "${KUBEHENGE_E2E_NODE_OS:-}" ] || fail "Talos nodes run Talos, not ${KUBEHENGE_E2E_NODE_OS}"
    talos_from=${KUBEHENGE_E2E_TALOS_VERSION:?the Talos release the cluster starts on}
    talos_to=${KUBEHENGE_E2E_TALOS_UPGRADE:?the Talos release its nodes are upgraded to}
    talos_variable=$(printf '      - name: talosVersion\n        value: %s\n' "${talos_from}")
    echo "nodes on Talos ${talos_from}"
else
    node_os=$(kubectl -n kubehenge-system get clusterclass "${class}" -o json |
        jq -r '.spec.variables[] | select(.name == "nodeOS") | .schema.openAPIV3Schema.default | "\(.family)-\(.release)"')
    if [ -n "${KUBEHENGE_E2E_NODE_OS:-}" ]; then
        node_os=${KUBEHENGE_E2E_NODE_OS}
        cni_variable=$(printf '%s\n      - name: nodeOS\n        value: {family: %s, release: "%s"}\n' "${cni_variable}" "${node_os%%-*}" "${node_os#*-}")
    fi
    echo "nodes on ${node_os}"
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
        $node.worker + $addon.headlamp + $addon.monitoring + $addon.logs + $addon.backups + $addon.kyverno + $addon.istio + $addon.loadBalancer + $addon.certManager + $addon.argocd + $addon.rancherAgent]
    | max')}
# Its cores likewise, for the CPU a worker's own pods and every add-on's request, the GitOps engine
# that asks for more of the two.
worker_cores=${KUBEHENGE_E2E_WORKER_CORES:-$(printf '%s' "${class_json}" | jq -r '
    (.metadata.annotations["kubehenge.dev/addon-cpu"] | fromjson) as $addon
    | (.metadata.annotations["kubehenge.dev/node-cpu"] | fromjson) as $node
    | [(.spec.variables[] | select(.name == "workerMachine") | .schema.openAPIV3Schema.properties.cores.minimum),
        (($node.worker + $addon.headlamp + $addon.monitoring + $addon.logs + $addon.backups + $addon.kyverno + $addon.istio + $addon.loadBalancer + $addon.certManager + ([$addon.argocd, $addon.flux] | max) + $addon.rancherAgent) / 1000 | ceil)]
    | max')}
# The control plane's memory likewise: the least the class's sizing guide gives it beside one worker, or what
# it uses itself with every add-on but Rancher, whichever is more.
control_plane_memory=${KUBEHENGE_E2E_CONTROL_PLANE_MEMORY:-$(printf '%s' "${class_json}" | jq -r '
    (.metadata.annotations["kubehenge.dev/addon-control-plane-memory"] // "{}" | fromjson) as $grows
    | (.metadata.annotations["kubehenge.dev/node-memory"] | fromjson) as $node
    | [(.metadata.annotations["kubehenge.dev/control-plane-sizes"] | fromjson | map(select(.workers <= 1)) | last | .memoryMiB),
        (.spec.variables[] | select(.name == "controlPlaneMachine") | .schema.openAPIV3Schema.properties.memoryMiB.minimum),
        $node.controlPlane + ([$grows.headlamp, $grows.monitoring, $grows.logs, $grows.backups, $grows.kyverno, $grows.istio, $grows.loadBalancer, $grows.certManager, $grows.argocd, $grows.rancherAgent] | map(. // 0) | add)]
    | max')}
echo "workers with ${worker_memory} MiB of memory and ${worker_cores} cores, the control plane with ${control_plane_memory} MiB"
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
        value: {memoryMiB: ${control_plane_memory}, cores: ${KUBEHENGE_E2E_CONTROL_PLANE_CORES:-2}}
      - name: workerMachine
        value: {memoryMiB: ${worker_memory}, cores: ${worker_cores}}
${cni_variable}
${talos_variable}
      - name: oidc
        value: ${oidc}
      # The strictest level from the start, so that every component Kubehenge installs is seen to
      # run under it, or to sit in a namespace it exempts, and every add-on to work hardened.
      - name: podSecurity
        value: {admission: restricted}
      - name: cis
        value: ${cis}
      # The test upgrades the cluster itself. Talos's automatic patch upgrades would start whenever a
      # run reaches the default window, 02:00 to 06:00 UTC, since its nodes start a patch behind.
      - name: upgrades
        value: {mode: "off", nodeImages: false}
EOF
creator create -f "${work}/cluster.yaml"
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
# The Cluster's own, or else the class's defaults, as Kubehenge's controller reads them. Only the
# defaults go to jq as an argument: the whole class is larger than Linux allows one argument to be.
class_ranges=$(printf '%s' "${class_json}" | jq -c '.metadata.annotations |
    {pods: [.["kubehenge.dev/default-pods"]], services: [.["kubehenge.dev/default-services"]]}')
ranges=$(kubectl -n "${ns}" get "cluster/${name}" -o json | jq -c --argjson class "${class_ranges}" '{
    pods: (.spec.clusterNetwork.pods.cidrBlocks // $class.pods),
    services: (.spec.clusterNetwork.services.cidrBlocks // $class.services)}')
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

if [ "${distribution}" = talos ]; then
    # talos_addresses: the address of each of the cluster's machines, where its Talos API answers.
    talos_addresses() {
        kubectl -n "${ns}" get machines -l "cluster.x-k8s.io/cluster-name=${name}" -o json |
            jq -r '.items[] | [.status.addresses[]? | select(.type == "InternalIP") | .address][0] // empty'
    }
    # machine_address <Machine name>: that machine's address.
    machine_address() {
        kubectl -n "${ns}" get machine "$1" -o json | jq -r '[.status.addresses[]? | select(.type == "InternalIP") | .address][0] // empty'
    }
    # talos_api <address> <talosctl arguments...>: the Talos API of the node at the address, with the
    # cluster's talosconfig, asked directly rather than through a control plane.
    talos_api() {
        talos_address=$1
        shift
        talosctl --talosconfig "${work}/talosconfig" --endpoints "${talos_address}" --nodes "${talos_address}" "$@"
    }
    # talos_read <address> <file...>: each line of the files on the node after its file's name, as
    # grep -H prints them where the guest agent reads a node.
    talos_read() {
        read_address=$1
        shift
        for read_file in "$@"; do
            # shellcheck disable=SC2310 # its failure is returned.
            read_lines=$(talos_api "${read_address}" read "${read_file}") || return 1
            printf '%s\n' "${read_lines}" | sed "s|^|${read_file}:|"
        done
    }
    # talos_cpu <address>: the CPU the node sees, as the guest agent reads it elsewhere: its model,
    # whether it has PCID, and the kernel's word on each vulnerability.
    talos_cpu() {
        # shellcheck disable=SC2310 # its failure is returned.
        cpu_info=$(talos_api "$1" read /proc/cpuinfo) || return 1
        printf '%s\n' "${cpu_info}" | grep -m1 'model name'
        if printf '%s\n' "${cpu_info}" | grep -qw pcid; then echo 'pcid: yes'; else echo 'pcid: no'; fi
        # shellcheck disable=SC2310 # its failure is returned.
        cpu_flaws=$(talos_api "$1" list /sys/devices/system/cpu/vulnerabilities) || return 1
        cpu_files=$(printf '%s\n' "${cpu_flaws}" | awk 'NR > 1 && $2 != "." { print "/sys/devices/system/cpu/vulnerabilities/" $2 }')
        # shellcheck disable=SC2086 # one file per word
        talos_read "$1" ${cpu_files}
    }
    # talos_release <address>: the Talos release the node at the address says it runs.
    talos_release() {
        # shellcheck disable=SC2310 # its failure is returned.
        talos_version=$(talos_api "$1" version) || return 1
        printf '%s\n' "${talos_version}" | awk '/^Server:/ { server = 1 } server && $1 == "Tag:" { print $2 }'
    }
    # machines_on_talos <release>: every Machine's node is Ready, its kubelet reporting the release.
    machines_on_talos() {
        talos_machines=$(kubectl -n "${ns}" get machines -l "cluster.x-k8s.io/cluster-name=${name}" -o json) || return 1
        printf '%s' "${talos_machines}" | jq -e --arg os "Talos ($1)" '.items | length > 0 and all(.status.nodeInfo.osImage == $os and
            any(.status.conditions[]?; .type == "NodeReady" and .status == "True"))' > /dev/null
    }
    # talos_annotation <name>: the Cluster's kubehenge.dev/<name> annotation, empty without one.
    talos_annotation() {
        kubectl -n "${ns}" get "cluster/${name}" -o json | jq -r --arg a "kubehenge.dev/$1" '.metadata.annotations[$a] // ""'
    }

    step "the nodes run Talos ${talos_from}, reached through Talos's API with the cluster's talosconfig"
    talos_image=$(talos_annotation talos-image)
    [ "${talos_image}" = "${talos_from}" ] || fail "the cluster's machines boot Talos '${talos_image}', not ${talos_from}"
    # shellcheck disable=SC2310 # a failed read fails the step.
    machines_on_talos "${talos_from}" || fail "the machines' kubelets do not all report Talos ${talos_from}"
    # As the interface hands it out: the Secret Talos's providers keep beside the cluster.
    talosconfig=$(kubectl -n "${ns}" get secret "${name}-talosconfig" -o jsonpath='{.data.talosconfig}')
    [ -n "${talosconfig}" ] || fail "the cluster has no talosconfig"
    (
        umask 077
        printf '%s' "${talosconfig}" | base64 -d > "${work}/talosconfig"
    )
    talos_info=$(talosctl --talosconfig "${work}/talosconfig" config info -o json) || fail "the talosconfig does not parse"
    printf '%s' "${talos_info}" | jq -e '.context != "" and any(.roles[]?; . == "os:admin")' > /dev/null ||
        fail "the talosconfig has no context, or no administrator's certificate: ${talos_info}"
    for address in $(talos_addresses); do
        # shellcheck disable=SC2310 # a node that does not answer fails the step.
        release=$(talos_release "${address}") || fail "the Talos API of ${address} does not answer with the talosconfig"
        [ "${release}" = "${talos_from}" ] || fail "the Talos API of ${address} reports '${release}', not ${talos_from}"
    done
fi

# kube-bench carries no benchmark for Talos, whose components keep their configuration where none
# of its checks look.
if [ "${distribution}" != talos ] && [ "${cis}" = true ]; then
    step "the nodes pass the CIS Kubernetes Benchmark"
    cis_benchmark
fi

step "the cluster runs its CNI, which enforces network policies"
cni=$(kubectl -n "${ns}" get "cluster/${name}" -o json | jq -r '.spec.topology.variables[] | select(.name == "cni") | .value')
case ${cni} in
    cilium) wl -n kube-system rollout status daemonset/cilium --timeout=10m ;;
    calico) until_true 600 "calico-node" sh -c "KUBECONFIG=${work}/workload kubectl -n calico-system rollout status daemonset/calico-node --timeout=10s >/dev/null 2>&1" ;;
    canal) wl -n kube-system rollout status daemonset/rke2-canal --timeout=10m ;;
    # k3s's flannel runs inside k3s itself, Talos's as a DaemonSet of its own.
    flannel) [ "${distribution}" != talos ] || wl -n kube-system rollout status daemonset/kube-flannel --timeout=10m ;;
    *) fail "no check for CNI ${cni}" ;;
esac
case ${cni} in
    cilium | calico)
        nodes=$(wl get nodes -o json)
        printf '%s' "${nodes}" | jq -e '.items | all(.metadata.annotations | keys | any(startswith("flannel.alpha.coreos.com/")) | not)' > /dev/null ||
            fail "flannel runs beside ${cni}"
        ;;
    # Canal, and k3s's and Talos's own CNI, carry flannel by design.
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
# agnhost_deployment <namespace> <name> <args...>: one agnhost pod as agnhost_pod makes it, kept by
# a Deployment labelled app=<name>, for a pod that has to outlive a drain of its node. Its pods take
# <name> as their host name, which netexec answers with.
agnhost_deployment() {
    agnhost_namespace=$1 agnhost_name=$2
    shift 2
    jq -n --arg name "${agnhost_name}" --arg image "${agnhost}" '$ARGS.positional as $args | {
        apiVersion: "apps/v1", kind: "Deployment", metadata: {name: $name},
        spec: {replicas: 1, selector: {matchLabels: {app: $name}}, template: {
            metadata: {labels: {app: $name}},
            spec: {hostname: $name,
                securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: "RuntimeDefault"}},
                containers: [{name: $name, image: $image, args: $args,
                    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}]}}}}' --args -- "$@" |
        wl -n "${agnhost_namespace}" apply -f -
}
new_namespace wl e2e-policy
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
# identity_kubeconfig <health | health-logs | headlamp-issuer>: a kubeconfig of the token
# Kubehenge's controller keeps beside the cluster, written to a file only this user reads rather
# than passed as an argument.
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
new_namespace wl e2e-health
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

step "the shared Headlamp's sign-in identities, only for the interface's namespaces"
headlamp_namespaces=$(kubectl -n kubehenge-system get deployment/kubehenge-controller -o json |
    jq -r '[.spec.template.spec.containers[0].args[] | select(startswith("--headlamp-sign-in-namespaces=")) | split("=")[1] | split(",")[]] | join(" ")')
signed_in=
case " ${headlamp_namespaces} " in
    *" ${ns} "*) signed_in=yes ;;
    *) ;;
esac
if [ -n "${signed_in}" ]; then
    until_true 300 "the Headlamp issuer's token beside the cluster" identity_kubeconfig headlamp-issuer
    # The issuer makes the Headlamp account's tokens, as the interface does for a session, and
    # nothing else; a token made acts as cluster-admin.
    (
        umask 077
        KUBECONFIG="${work}/headlamp-issuer" kubectl -n kube-system create token kubehenge-headlamp --duration=10m > "${work}/headlamp-token" ||
            fail "the issuer cannot make the Headlamp account's token"
        KUBECONFIG="${work}/headlamp-issuer" kubectl config view --raw -o json |
            jq --rawfile t "${work}/headlamp-token" '.users[0].user.token = ($t | rtrimstr("\n"))' > "${work}/headlamp-admin"
    )
    admin_can=$(KUBECONFIG="${work}/headlamp-admin" kubectl auth can-i '*' '*' --all-namespaces || true)
    [ "${admin_can}" = yes ] || fail "the Headlamp account's token is not cluster-admin"
    refused headlamp-issuer get secrets -n kube-system
    refused headlamp-issuer list pods --all-namespaces
    refused headlamp-issuer create serviceaccounts/kubehenge-health --subresource=token -n kube-system
    refused headlamp-issuer create serviceaccounts/kubehenge-headlamp-issuer --subresource=token -n kube-system
    mounted=$(wl -n kube-system get serviceaccount kubehenge-headlamp -o jsonpath='{.automountServiceAccountToken}')
    [ "${mounted}" = false ] || fail "the Headlamp account's token is mounted into pods"
else
    found=$(wl -n kube-system get serviceaccount kubehenge-headlamp -o name --ignore-not-found)
    [ -z "${found}" ] ||
        fail "the Headlamp account is in a cluster outside the interface's namespaces (${headlamp_namespaces:-sign-in off})"
    found=$(kubectl -n "${ns}" get "secret/${name}-headlamp-issuer" -o name --ignore-not-found)
    [ -z "${found}" ] ||
        fail "the Headlamp issuer's token is beside a cluster outside the interface's namespaces"
fi

step "the ingress controller serves a host name on the API address and every node"
new_namespace wl e2e-ingress
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
new_namespace wl e2e-lb
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

if [ "${KUBEHENGE_E2E_VOLUME_SNAPSHOTS:-}" = 1 ]; then
    kubectl -n kubehenge-system get secret kubehenge-root-pam > /dev/null ||
        fail "no root@pam password is stored for volume snapshots"
    # csi_controller_is <Secret> <true|false>: the CSI plugin's controller is rolled out reading its
    # configuration from the Secret, with the snapshot sidecar or without.
    csi_controller_is() {
        # shellcheck disable=SC2310 # polled; a failed read is a no.
        deployments=$(wl -n kube-system get deployments -o json) || return 1
        printf '%s' "${deployments}" | jq -e --arg s "$1" --argjson snap "$2" '
            [.items[] | select(any(.spec.template.spec.volumes[]?; .name == "cloud-config" and .secret.items[0].key == "csi.yaml"))] |
            length == 1 and all(
                ([.spec.template.spec.volumes[] | select(.name == "cloud-config") | .secret.secretName][0] == $s) and
                (any(.spec.template.spec.containers[]; .name == "csi-snapshotter") == $snap) and
                (.status.observedGeneration // 0) >= .metadata.generation and
                (.status.updatedReplicas // 0) == .spec.replicas and (.status.readyReplicas // 0) == .spec.replicas and
                (.status.replicas // 0) == .spec.replicas)' > /dev/null
    }
    # signs_in_as_root: the plugin's root configuration names root@pam and no token; its password is
    # never printed.
    signs_in_as_root() {
        # shellcheck disable=SC2310 # polled; a failed read is a no.
        config=$(wl -n kube-system get secret kubehenge-proxmox-root -o jsonpath='{.data.csi\.yaml}' 2> /dev/null) || return 1
        decoded=$(printf '%s' "${config}" | base64 -d)
        printf '%s\n' "${decoded}" | grep -qx '  username: root@pam' && ! printf '%s\n' "${decoded}" | grep -q token_
    }
    # not_found <kubectl get arguments>: the object is not found, rather than out of reach for a
    # moment.
    not_found() {
        # shellcheck disable=SC2310 # the request failing is the answer looked for.
        if answer=$(wl get "$@" 2>&1); then
            return 1
        fi
        case ${answer} in
            *NotFound* | *"not found"*) ;;
            *) return 1 ;;
        esac
    }
    # none_of <kubectl get arguments>: the list is read, and empty.
    none_of() {
        # shellcheck disable=SC2310 # polled; a failed read is a no.
        names=$(wl get "$@" -o name) && [ -z "${names}" ]
    }
    snapshot_ready() {
        # shellcheck disable=SC2310 # polled; a failed read is a no.
        ready=$(wl -n default get volumesnapshot e2e -o jsonpath='{.status.readyToUse}') && [ "${ready}" = true ]
    }
    no_owned_volumes() {
        # shellcheck disable=SC2310 # polled; a failed read is a no.
        volumes=$(owned) && [ "${volumes}" = 0 ]
    }
    # data_pod <name> <claim> <command>: runs a shell command against a volume, once.
    data_pod() {
        wl apply -f - << EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: default
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: data
      image: ${agnhost}
      command: [sh, -ec, "$3"]
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
        claimName: $2
EOF
        # shellcheck disable=SC2310 # a pod that did not succeed fails the step, with its last lines.
        if ! wl -n default wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$1" --timeout=10m; then
            last=$(wl -n default logs "pod/$1" 2>&1 | tail -n 5) || true
            fail "pod $1 did not succeed: ${last}"
        fi
    }

    step "volume snapshots, with the CSI plugin signed in as root@pam"
    set_variable csi '{"volumeSnapshots":true}'
    until_true 300 "the CSI plugin's root@pam configuration" signs_in_as_root
    until_true 900 "the CSI plugin's controller with the snapshot sidecar, signed in as root@pam" csi_controller_is kubehenge-proxmox-root true
    until_true 600 "the VolumeSnapshotClass" wl get volumesnapshotclass proxmox
    if [ "${distribution}" != rke2 ]; then
        wl -n kube-system rollout status deployment -l app.kubernetes.io/name=snapshot-controller --timeout=5m
    fi
    wl apply -f - << 'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: e2e-source
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
EOF
    # 64 MiB of random data and its checksum, written and flushed, and the volume detached again
    # before its snapshot is taken.
    data_pod e2e-write e2e-source 'head -c 67108864 /dev/urandom > /data/blob; cd /data; sha256sum blob > sum; sync'
    wl -n default delete pod/e2e-write --wait --timeout=5m
    wl apply -f - << 'EOF'
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: e2e
  namespace: default
spec:
  volumeSnapshotClassName: proxmox
  source:
    persistentVolumeClaimName: e2e-source
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: e2e-restored
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: e2e
  resources:
    requests:
      storage: 1Gi
EOF
    until_true 900 "the volume snapshot ready to use" snapshot_ready
    data_pod e2e-check e2e-restored 'cd /data; sha256sum -c sum'
    wl -n default delete pod/e2e-check pvc/e2e-restored pvc/e2e-source volumesnapshot/e2e --wait --timeout=5m
    # The CSI plugin retries a failed deletion with a back-off of up to five minutes.
    until_true 600 "the snapshot's content deleted" none_of volumesnapshotcontents
    until_true 600 "the volumes deleted" sh -c "[ \"\$(KUBECONFIG=${work}/workload kubectl get pv -o name | wc -l)\" = 0 ]"
    until_true 600 "no volume left on ${KUBEHENGE_E2E_STORAGE}" no_owned_volumes

    step "the CSI plugin with the cluster's token again, without volume snapshots"
    set_variable csi '{"volumeSnapshots":false}'
    until_true 900 "the CSI plugin's controller without the snapshot sidecar, with the cluster's token" csi_controller_is kubehenge-proxmox false
    until_true 300 "the CSI plugin's root@pam configuration deleted" not_found -n kube-system secret kubehenge-proxmox-root
    until_true 600 "the VolumeSnapshotClass removed" not_found volumesnapshotclass proxmox
    until_true 600 "the snapshot controller removed" none_of -n kube-system deployments -l app.kubernetes.io/name=snapshot-controller
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

    step "nodes labelled with their HA rule, with Sys.Audit on / for the cluster's token by default"
    # The cluster was created without its ccm variable, so it has what the class defaults it to.
    ccm=$(kubectl -n "${ns}" get "cluster/${name}" -o json) || fail "cannot read cluster ${name}"
    ccm=$(printf '%s' "${ccm}" | jq -c '.spec.topology.variables[] | select(.name == "ccm") | .value')
    [ "${ccm}" = '{"haGroups":true}' ] ||
        fail "the class defaulted the cluster's ccm variable to ${ccm:-nothing}, not the HA group labels"
    until_true 300 "the cluster's token to hold Sys.Audit on /" audit_is true
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
    # The cloud controller manager labels the nodes when it next syncs them.
    until_true 900 "every node labelled with HA rule ${ha_rule}" labelled
    wl get nodes -L "group.topology.proxmox.sinextra.dev/${ha_rule}"

    step "the cluster's token without Sys.Audit on / once switched off, and with it again once on"
    set_variable ccm '{"haGroups":false}'
    until_true 300 "the cluster's token to lose Sys.Audit on /" audit_is false
    set_variable ccm '{"haGroups":true}'
    until_true 300 "the cluster's token to hold Sys.Audit on / again" audit_is true
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

# etcd is taken, restored and uploaded before any add-on runs: replacing a server holds two beside
# the worker, which the runner has room for only while the cluster is this small, and every add-on
# after it then runs on a restored cluster.
step "an S3 store for the backups and etcd snapshots"
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
# As the interface stores a cluster's own key for its target.
kubectl -n "${ns}" create secret generic "${name}.backup" --type=kubehenge.dev/user-credential \
    --from-literal=accessKeyID="${s3_key}" --from-literal=secretAccessKey="${s3_secret}"
kubectl -n "${ns}" label secret "${name}.backup" kubehenge.dev/user-credential=backup "kubehenge.dev/credential-cluster=${name}"

step "etcd snapshots: one taken into the S3 store, encrypted, and read back"
# A ConfigMap rather than a Secret, which the cluster encrypts in etcd: the database holds it in
# plain text, so it is seen there and must not be seen in the store.
etcd_marker=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
wl -n default create configmap e2e-before-snapshot --from-literal=marker="${etcd_marker}"
# Once a day, twelve hours from now: the default schedule's slot after the cluster was made can have
# passed already, and switching snapshots on would then take one of its own beside the one asked for.
etcd_schedule="0 $((($(date -u +%-H) + 12) % 24)) * * *"
set_variable etcdSnapshots "{\"enabled\":true,\"target\":\"${KUBEHENGE_E2E_BACKUP_TARGET}\",\"keep\":2,\"schedule\":\"${etcd_schedule}\"}"
# As the interface's Take a snapshot now asks for one.
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kubectl -n "${ns}" annotate cluster "${name}" --overwrite \
    "kubehenge.dev/etcd-snapshot-requested={\"by\":\"e2e\",\"at\":\"${now}\"}"
# snapshot_taken: whether the controller has taken the snapshot asked for; one it tried and failed
# fails the test with why.
snapshot_taken() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    cluster_json=$(kubectl -n "${ns}" get cluster "${name}" -o json) || return 1
    if printf '%s' "${cluster_json}" | jq -e '.metadata.annotations["kubehenge.dev/etcd-snapshot-requested"]' > /dev/null; then
        return 1
    fi
    if ! printf '%s' "${cluster_json}" |
        jq -e '.status.conditions[]? | select(.type == "EtcdSnapshotSucceeded" and .status == "True")' > /dev/null; then
        printf '%s' "${cluster_json}" | jq '.status.conditions[]? | select(.type == "EtcdSnapshotSucceeded")' >&2
        kubectl -n kubehenge-system logs deployment/kubehenge-controller --tail=100 >&2
        fail "the etcd snapshot was not taken"
    fi
}
until_true 900 "an etcd snapshot taken" snapshot_taken
snapshot=$(kubectl -n "${ns}" get cluster "${name}" -o json | jq -c '.metadata.annotations["kubehenge.dev/etcd-snapshot"] | fromjson')
snapshot_name=$(printf '%s' "${snapshot}" | jq -r .name)
etcd_prefix=$(printf '%s' "${target}" | jq -r --arg own "${ns}.${name}/etcd" 'if (.prefix // "") == "" then $own else .prefix + "/" + $own end')
keys=$(s3 "${s3_endpoint}/${bucket}?list-type=2&max-keys=1000&prefix=${etcd_prefix}/" | grep -o '<Key>[^<]*</Key>' | sed 's/<[^>]*>//g')
[ "${keys}" = "${etcd_prefix}/${snapshot_name}" ] || fail "the store's etcd snapshots are not the one taken: ${keys}"
s3 -o "${work}/snapshot.db.age" "${s3_endpoint}/${bucket}/${etcd_prefix}/${snapshot_name}"
header=$(head -c 21 "${work}/snapshot.db.age")
[ "${header}" = age-encryption.org/v1 ] || fail "the etcd snapshot in the store is not encrypted with age"
grep -r -q -F "${etcd_marker}" "${work}/s3-data" && fail "the store keeps the etcd snapshot unencrypted"
kubectl -n "${ns}" get secret "${name}-backup-keys" -o jsonpath='{.data.etcd-snapshot-identity}' | base64 -d > "${work}/etcd-identity"
go -C "$(dirname "$0")/../.." run ./cmd/kubehenge etcd-snapshot decrypt --identity "${work}/etcd-identity" \
    "${work}/snapshot.db.age" "${work}/snapshot.db"
sum=$(sha256sum "${work}/snapshot.db" | cut -d ' ' -f 1)
taken_sum=$(printf '%s' "${snapshot}" | jq -r .sha256)
[ "${sum}" = "${taken_sum}" ] || fail "the decrypted snapshot does not have the SHA-256 it was taken with"
curl -sS --fail -L -o "${work}/etcd.tar.gz" \
    "https://github.com/etcd-io/etcd/releases/download/${ETCD_VERSION}/etcd-${ETCD_VERSION}-linux-amd64.tar.gz"
printf '%s  %s\n' "${ETCD_SHA256}" "${work}/etcd.tar.gz" | sha256sum -c - > /dev/null ||
    fail "etcd's download does not have its SHA-256"
tar -xzf "${work}/etcd.tar.gz" -C "${work}" --strip-components=1 "etcd-${ETCD_VERSION}-linux-amd64/etcdutl"
status=$("${work}/etcdutl" snapshot status "${work}/snapshot.db" -w json) || fail "etcdutl cannot read the snapshot"
if [ "${distribution}" = talos ]; then
    # Talos's API streams the snapshot without telling the store's revision.
    printf '%s' "${snapshot}" | jq -e 'has("revision") | not' > /dev/null ||
        fail "the snapshot records a revision Talos never told: ${snapshot}"
else
    # The revision recorded is the member's just before the snapshot began; writes in between add
    # to the snapshot's.
    db_revision=$(printf '%s' "${status}" | jq .revision)
    taken_revision=$(printf '%s' "${snapshot}" | jq '.revision // 0')
    [ "${taken_revision}" -gt 0 ] || fail "no revision was recorded for the snapshot: ${snapshot}"
    [ "${db_revision}" -ge "${taken_revision}" ] ||
        fail "the snapshot is older than the revision recorded for it: ${status}"
fi
grep -a -q -F "${etcd_marker}" "${work}/snapshot.db" || fail "the snapshot does not hold the ConfigMap made before it"

# lease_times: each Lease's last renewal, by namespace/name. An API server's identity Lease names one
# process and outlives it by an hour, and the nodes' Leases go with their nodes, so neither is in.
lease_times() {
    wl get leases -A -l '!apiserver.kubernetes.io/identity' -o json |
        jq -c '[.items[] | select(.metadata.namespace != "kube-node-lease") |
            {key: "\(.metadata.namespace)/\(.metadata.name)", value: (.spec.renewTime // "")}] | from_entries'
}
# etcd_restore_requested: asks for a restore from the snapshot, as the interface's Restore does,
# noting first the Leases the cluster's components renew, which the restart baseline waits on.
etcd_restore_requested() {
    held_from=$(date +%s)
    held_since=$(date -u -d "@$((held_from - 60))" +%Y-%m-%dT%H:%M:%SZ)
    held_leases=$(lease_times | jq -c --arg since "${held_since}" '[to_entries[] | select(.value > $since) | .key]')
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    kubectl -n "${ns}" annotate cluster "${name}" \
        "kubehenge.dev/etcd-restore={\"snapshot\":\"${snapshot_name}\",\"target\":\"${KUBEHENGE_E2E_BACKUP_TARGET}\",\"by\":\"e2e\",\"at\":\"${now}\"}"
}
if [ "${distribution}" = talos ]; then
    step "etcd restored from the snapshot through the Talos API, the control planes wiped and joining it again"
else
    step "etcd restored from the snapshot through the guest agent"
fi
control_planes=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
wl -n default delete configmap e2e-before-snapshot
after_marker=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
wl -n default create configmap e2e-after-snapshot --from-literal=marker="${after_marker}"
etcd_restore_requested
# etcd_restored: whether the restore has ended and the controller let the cluster go; a failed one
# fails the test with why.
etcd_restored() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    cluster_json=$(kubectl -n "${ns}" get cluster "${name}" -o json) || return 1
    restore=$(printf '%s' "${cluster_json}" | jq -c '.metadata.annotations["kubehenge.dev/etcd-restore-status"] // "{}" | fromjson')
    case $(printf '%s' "${restore}" | jq -r '.phase // ""') in
        Succeeded) ;;
        Failed)
            kubectl -n kubehenge-system logs deployment/kubehenge-controller --tail=100 >&2
            fail "the etcd restore failed: ${restore}"
            ;;
        *) return 1 ;;
    esac
    printf '%s' "${cluster_json}" | jq -e '(.metadata.annotations["kubehenge.dev/etcd-restore"] | not) and (.spec.paused // false | not)' > /dev/null
}
until_true 2400 "etcd restored" etcd_restored
until_true 1200 "cluster ${name} available after the restore" available_or_failed
wl wait --for=condition=Ready nodes --all --timeout=10m
until_true 300 "the ConfigMap made before the snapshot back" wl -n default get configmap e2e-before-snapshot
# shellcheck disable=SC2310 # absent's answer is what is checked.
absent wl -n default get configmap e2e-after-snapshot || fail "a ConfigMap made after the snapshot outlived the restore"
wl -n default delete configmap e2e-before-snapshot
# The restore snapshots etcd as it was first, so that a restore from the wrong snapshot can be undone.
before_name=$(printf '%s' "${restore}" | jq -r '.before // ""')
[ -n "${before_name}" ] || fail "the restore took no snapshot of etcd as it was first: ${restore}"
keys=$(s3 "${s3_endpoint}/${bucket}?list-type=2&max-keys=1000&prefix=${etcd_prefix}/" | grep -o '<Key>[^<]*</Key>' | sed 's/<[^>]*>//g' | sort)
expected_keys=$(printf '%s/%s\n' "${etcd_prefix}" "${snapshot_name}" "${etcd_prefix}" "${before_name}" | sort)
[ "${keys}" = "${expected_keys}" ] ||
    fail "the store's etcd snapshots after the restore are not the one restored and the one of etcd as it was: ${keys}"
s3 -o "${work}/before.db.age" "${s3_endpoint}/${bucket}/${etcd_prefix}/${before_name}"
header=$(head -c 21 "${work}/before.db.age")
[ "${header}" = age-encryption.org/v1 ] || fail "the snapshot of etcd as it was is not encrypted with age"
grep -r -q -F "${after_marker}" "${work}/s3-data" && fail "the store keeps the snapshot of etcd as it was unencrypted"
go -C "$(dirname "$0")/../.." run ./cmd/kubehenge etcd-snapshot decrypt --identity "${work}/etcd-identity" \
    "${work}/before.db.age" "${work}/before.db"
grep -a -q -F "${after_marker}" "${work}/before.db" ||
    fail "the snapshot of etcd as it was does not hold the ConfigMap made before the restore"
if [ "${distribution}" = talos ]; then
    # Talos's control planes join the restored etcd again rather than being made anew.
    restored_control_planes=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
    [ "${restored_control_planes}" = "${control_planes}" ] ||
        fail "the restore replaced control planes: ${control_planes} became ${restored_control_planes}"
    first_control_plane=$(machine_address "${control_planes%% *}")
    # etcd_rejoined: whether every control plane is a voting member of the restored etcd again.
    etcd_rejoined() {
        # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
        members=$(talos_api "${first_control_plane}" etcd members) || return 1
        voting=$(printf '%s\n' "${members}" | awk 'NR > 1 && $NF == "false" { n++ } END { print n + 0 }')
        wanted=$(printf '%s' "${control_planes}" | wc -w)
        [ "${voting}" -eq "${wanted}" ]
    }
    until_true 900 "every control plane a member of the restored etcd" etcd_rejoined
fi

case ${distribution} in
    rke2 | k3s)
        step "${distribution}'s own etcd snapshots uploaded to the same store, and switched off"
        # The drop-in is part of the servers' bootstrap, so switching the upload replaces them.
        servers=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
        servers_replaced() {
            kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o json |
                jq -e --arg old " ${servers} " '(.items | length > 0) and all(.items[]; .metadata.name as $name |
                    .status.phase == "Running" and (($old | contains(" " + $name + " ")) | not))' > /dev/null
        }
        set_variable etcdSnapshots "{\"enabled\":true,\"target\":\"${KUBEHENGE_E2E_BACKUP_TARGET}\",\"keep\":2,\"schedule\":\"${etcd_schedule}\",\"distributionUpload\":true}"
        until_true 300 "the upload's Secret in kube-system" wl -n kube-system get secret kubehenge-etcd-s3
        upload_secret=$(wl -n kube-system get secret kubehenge-etcd-s3 -o json)
        printf '%s' "${upload_secret}" | jq -e '.type == "etcd.k3s.cattle.io/s3-config-secret" and
            (.data["etcd-s3-skip-ssl-verify"] | @base64d) == "false" and (.data["etcd-s3-insecure"] | @base64d) == "false" and
            (.data["etcd-s3-endpoint-ca"] | @base64d | length > 0)' > /dev/null ||
            fail "the upload's Secret does not verify the store's certificate"
        until_true 1800 "the control planes replaced for the upload" servers_replaced
        kubectl -n "${ns}" wait --for=condition=Available "cluster/${name}" --timeout=20m
        upload_prefix=$(printf '%s' "${target}" | jq -r --arg own "${ns}.${name}/${distribution}-etcd" 'if (.prefix // "") == "" then $own else .prefix + "/" + $own end')
        servers=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
        # The server reads the drop-in and the Secret, so the snapshot asked for goes to the store too.
        # shellcheck disable=SC2310 # a failed save fails the test with the reason.
        on_machines "${servers%% *}" "PATH=\$PATH:/usr/local/bin:/opt/rke2/bin ${distribution} etcd-snapshot save --name e2e-upload" > /dev/null ||
            fail "${distribution} could not take a snapshot of its own"
        uploaded() {
            # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
            listing=$(s3 "${s3_endpoint}/${bucket}?list-type=2&max-keys=1000&prefix=${upload_prefix}/") || return 1
            printf '%s' "${listing}" | grep -q "<Key>${upload_prefix}/e2e-upload"
        }
        until_true 300 "${distribution}'s snapshot in the store" uploaded
        servers=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
        set_variable etcdSnapshots "{\"enabled\":false,\"target\":\"${KUBEHENGE_E2E_BACKUP_TARGET}\"}"
        # The Secret stays until no server runs with the drop-in, which a server not yet replaced
        # still reads on each scheduled snapshot, so it goes only once the servers are replaced.
        until_true 1800 "the control planes replaced without the upload" servers_replaced
        kubectl -n "${ns}" wait --for=condition=Available "cluster/${name}" --timeout=20m
        until_true 300 "the upload's Secret gone" absent wl -n kube-system get secret kubehenge-etcd-s3
        ;;
    *) set_variable etcdSnapshots "{\"enabled\":false,\"target\":\"${KUBEHENGE_E2E_BACKUP_TARGET}\"}" ;;
esac

# restarts: each pod's container restarts, by namespace/name.
restarts() {
    wl get pods -A -o json |
        jq -c '[.items[] | {key: "\(.metadata.namespace)/\(.metadata.name)", value: ([.status.containerStatuses[]?.restartCount] | add // 0)}] | from_entries'
}
# server_restarts: how often systemd has restarted the control planes' distribution service. k3s runs
# every control-plane component in it, so it is the one place k3s's restarts show. Talos has no
# systemd: its control planes' components are the kubelet's static pods, counted with the others.
case ${class} in
    kubehenge-rke2) server_unit=rke2-server ;;
    kubehenge-k3s) server_unit=k3s ;;
    kubehenge-talos) server_unit= ;;
    *) server_unit=kubelet ;;
esac
servers=$(kubectl -n "${ns}" get machines -l cluster.x-k8s.io/control-plane -o jsonpath='{.items[*].metadata.name}')
server_restarts() {
    # shellcheck disable=SC2310 # its failure is returned.
    counts=$(on_machines "${servers}" "systemctl show -p NRestarts --value ${server_unit}") || return 1
    printf '%s\n' "${counts}" | awk '{ n += $1 } END { print n + 0 }'
}
# A component that could not renew its Lease while the API was away, through the restore or a
# control plane's replacement, exits only once its renew deadline has passed, which may be after the
# API is back. Once each Lease renewed before the restore is renewed again after this point, every
# such restart has happened and is in the baseline.
leases_at=$(lease_times)
leases_renewed() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    times=$(lease_times) || return 1
    printf '%s' "${times}" | jq -e --argjson held "${held_leases}" --argjson at "${leases_at}" \
        '. as $now | all($held[]; ($now[.] // "") > ($at[.] // ""))' > /dev/null
}
until_true 300 "the components' Leases renewed after the restore" leases_renewed
pods_before=$(restarts)
if [ -n "${server_unit}" ]; then
    # shellcheck disable=SC2310 # a failed count fails the step.
    servers_before=$(server_restarts) || fail "cannot read ${server_unit}'s restarts on the control planes"
fi
echo "memory before the add-ons:"
pod_memory "${work}/workload"
echo "CPU requested before the add-ons:"
pod_cpu "${work}/workload"
# What the guests are told of the CPU decides which mitigations their kernels run, and those cost
# the most under nested virtualisation: the figure to read beside a slow run.
echo "the CPU a control plane sees:"
if [ "${distribution}" = talos ]; then
    cpu_address=$(machine_address "${servers%% *}")
    # shellcheck disable=SC2310 # a node that cannot be read is said so, not fatal here.
    talos_cpu "${cpu_address}" || echo "  cannot read it"
else
    # shellcheck disable=SC2310 # a node that cannot be read is said so, not fatal here.
    on_machines "${servers%% *}" "grep -m1 'model name' /proc/cpuinfo; grep -qw pcid /proc/cpuinfo && echo 'pcid: yes' || echo 'pcid: no'; grep -H . /sys/devices/system/cpu/vulnerabilities/*" ||
        echo "  cannot read it"
fi

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
# headlamp_pods: Headlamp's answer to a list of pods in its in-cluster context, asked through the
# API server's service proxy. The admin kubeconfig signs in with a client certificate, which ends
# at the API server, so Headlamp is sent no credentials of anyone's.
headlamp_pods() { wl get --raw '/api/v1/namespaces/headlamp/services/http:headlamp:80/proxy/clusters/main/api/v1/pods?limit=1' 2>&1; }
headlamp_lists_pods() { headlamp_pods | jq -e '.kind == "PodList"' > /dev/null 2>&1; }
# headlamp_refuses: whether the API server refused Headlamp's request, rather than Headlamp being
# out of reach for a moment.
headlamp_refuses() {
    # shellcheck disable=SC2310 # the request failing is the answer looked for.
    if answer=$(headlamp_pods); then
        seen="listed: $(printf '%s' "${answer}" | tr '\n' ' ' | cut -c1-400)"
        return 1
    fi
    seen="refused: $(printf '%s' "${answer}" | tr '\n' ' ' | cut -c1-400)"
    case ${answer} in
        *Unauthorized* | *Forbidden* | *forbidden*) ;;
        *) return 1 ;;
    esac
}
# headlamp_flag <present|absent>: whether Headlamp's running Deployment uses its own account.
headlamp_flag() {
    # shellcheck disable=SC2310 # a Deployment not read yet is no answer, polled again.
    args=$(wl -n headlamp get deployment/headlamp -o jsonpath='{.spec.template.spec.containers[0].args}') || return 1
    case ${args} in
        *-unsafe-use-service-account-token*) [ "$1" = present ] ;;
        *) [ "$1" = absent ] ;;
    esac
}
# headlamp_can <verb> <resource>: what Headlamp's service account may do across the cluster.
headlamp_can() {
    # shellcheck disable=SC2310 # can-i exits non-zero for its answer no, which the caller reads.
    wl auth can-i "$1" "$2" --all-namespaces --as=system:serviceaccount:headlamp:headlamp 2> /dev/null || true
}
until_true 120 "Headlamp refuses to list pods without a token" headlamp_refuses
control_plane_use "${work}/workload" headlamp

step "Headlamp read-only for everyone"
set_variable headlamp '{"enabled":true,"access":"view"}'
until_true 600 "Headlamp started with its own account" headlamp_flag present
wl -n headlamp rollout status deployment/headlamp --timeout=10m
until_true 120 "Headlamp lists pods without a token" headlamp_lists_pods
can_list=$(headlamp_can list pods)
[ "${can_list}" = yes ] || fail "Headlamp's service account cannot list pods"
for denied in "get secrets" "list secrets" "create pods" "delete deployments" "list nodes"; do
    # shellcheck disable=SC2086 # the verb and the resource, split on purpose.
    allowed=$(headlamp_can ${denied})
    [ "${allowed}" = no ] || fail "Headlamp's service account may ${denied}"
done

step "Headlamp back to everyone's own token"
set_variable headlamp '{"enabled":true}'
until_true 600 "Headlamp started without its own account" headlamp_flag absent
wl -n headlamp rollout status deployment/headlamp --timeout=10m
can_list=$(headlamp_can list pods)
[ "${can_list}" = no ] || fail "Headlamp's service account keeps its view binding"
until_true 120 "Headlamp refuses to list pods without a token again" headlamp_refuses

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
new_namespace wl e2e-privileged
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
control_plane_use "${work}/workload" kyverno

# Before monitoring: on Cilium, switching Istio on restarts Cilium's agents and operator, and with
# monitoring's pods busy beside them a worker of the least size runs short of CPU.
step "Istio switched on, the mesh's traffic over mutual TLS"
set_variable istio '{"enabled":true}'
until_true 900 "istiod available" wl -n istio-system wait --for=condition=Available deployment/istiod --timeout=10s
until_true 600 "ztunnel on every node" wl -n istio-node rollout status daemonset/ztunnel --timeout=10s
until_true 600 "istio-cni on every node" wl -n istio-node rollout status daemonset/istio-cni-node --timeout=10s
new_namespace wl e2e-mesh
wl label namespace e2e-mesh istio.io/dataplane-mode=ambient
new_namespace wl e2e-plain
# Kept by Deployments: they are used again once every add-on is switched off, and on Talos the
# upgrade before that drains every node, which deletes pods of their own.
agnhost_deployment e2e-mesh server netexec --http-port=8080
agnhost_deployment e2e-mesh client pause
agnhost_deployment e2e-plain client pause
wl -n e2e-mesh wait --for=condition=Available deployment/server deployment/client --timeout=5m
wl -n e2e-plain wait --for=condition=Available deployment/client --timeout=5m
redirected() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    marks=$(wl -n e2e-mesh get pods -l 'app in (server,client)' -o jsonpath='{.items[*].metadata.annotations.ambient\.istio\.io/redirection}') &&
        [ "${marks}" = "enabled enabled" ]
}
until_true 120 "the mesh's pods redirected through ztunnel" redirected
# fetch <namespace>: the server's host name, fetched over HTTP by the namespace's client at the
# address of the server's running pod, which a drain moves.
fetch() {
    mesh_ip=$(KUBECONFIG=${work}/workload kubectl -n e2e-mesh get pods -l app=server --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].status.podIP}' 2>&1) &&
        KUBECONFIG=${work}/workload kubectl -n "$1" exec deployment/client -- curl -sS --max-time 5 "http://${mesh_ip}:8080/hostname" 2>&1
}
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
control_plane_use "${work}/workload" istio

step "monitoring switched on"
set_variable monitoring '{"enabled":true}'
prometheus=statefulset/prometheus-kube-prometheus-stack-prometheus
until_true 1200 "Prometheus installed" sh -c "KUBECONFIG=${work}/workload kubectl -n monitoring get ${prometheus} >/dev/null 2>&1"
wl -n monitoring rollout status "${prometheus}" --timeout=15m
# Every component's job, each scraped with its server verified; k3s's components are in its
# kubelet's metrics. Talos's controller manager and scheduler are scraped only on releases that pass
# them a serving certificate of the cluster CA; before those they sign their own, which nothing
# could verify.
jobs="apiserver kubelet coredns kube-state-metrics node-exporter kube-etcd"
case ${distribution} in
    k3s) ;;
    talos)
        jobs="${jobs} kube-proxy"
        control_plane_pods=$(wl -n kube-system get pods -l 'component in (kube-controller-manager,kube-scheduler)' -o json)
        if printf '%s' "${control_plane_pods}" |
            jq -e '(.items | length > 0) and all(.items[]; any(.spec.containers[].command[]?; startswith("--tls-cert-file=")))' > /dev/null; then
            jobs="${jobs} kube-controller-manager kube-scheduler"
        fi
        ;;
    *) jobs="${jobs} kube-controller-manager kube-scheduler kube-proxy" ;;
esac
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
control_plane_use "${work}/workload" monitoring

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
new_namespace wl e2e-logs
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
if [ "${distribution}" = talos ]; then
    # Talos keeps no journal: it sends its services' and the kernel's logs to the collector on its
    # node's loopback, and its API server's audit log lies in a directory of its own.
    collector=$(wl -n logging-node get daemonset/alloy-node -o json)
    audit_dir=$(printf '%s' "${collector}" | jq -r '.spec.template.spec.volumes[] | select(.name == "audit") | .hostPath.path')
    [ "${audit_dir}" = /var/log/audit/kube ] || fail "the collector reads the audit log from '${audit_dir}', not /var/log/audit/kube"
    printf '%s' "${collector}" | jq -e 'any(.spec.template.spec.volumes[]; .name == "journal") | not' > /dev/null ||
        fail "the collector mounts a journal Talos does not keep"
    until_true 300 "Talos's service logs in Loki" logged '{job="talos", unit="kubelet"}' ""
    until_true 300 "the nodes' kernel logs in Loki" logged '{job="talos", unit="kernel"}' ""
else
    until_true 300 "the nodes' journals in Loki" logged '{job="journal"}' ""
fi
# The API server audited the pod's creation, after the collector started reading the log.
until_true 300 "the API server's audit log in Loki" logged '{job="audit"} |= "e2e-logs"' '"name":"short"'
wl -n e2e-logs delete pod/short --wait --timeout=2m
pod_security=$(wl label --dry-run=server --overwrite namespace logging pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "Loki's and the events collector's pods do not meet Pod Security's restricted level: ${pod_security}" ;;
    *) ;;
esac
control_plane_use "${work}/workload" logs

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

step "backups with Velero: a volume and a Secret backed up, deleted and restored"
marker=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
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
new_namespace wl e2e-backup
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
control_plane_use "${work}/workload" backups

step "the backup in the S3 store: under the cluster's own prefix, encrypted"
keys=$(s3 "${s3_endpoint}/${bucket}?list-type=2&max-keys=1000" | grep -o '<Key>[^<]*</Key>' | sed 's/<[^>]*>//g')
# The etcd snapshots taken before stay in the store under the cluster's etcd prefixes.
stray=$(printf '%s\n' "${keys}" | grep -v -e "^${prefix}/" -e "^${etcd_prefix}/" -e "^${upload_prefix:-${etcd_prefix}}/" -e '^$' || true)
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

step "certificates switched on, from the cluster's own CA"
set_variable certificates '{"enabled":true,"issuer":"ca","domain":"e2e.test"}'
ingress_ca=${name}-ingress-ca
until_true 300 "the cluster's CA beside it" sh -c "kubectl -n ${ns} get secret ${ingress_ca} >/dev/null 2>&1"
ca_domain=$(kubectl -n "${ns}" get secret "${ingress_ca}" -o jsonpath='{.metadata.annotations.kubehenge\.dev/ingress-ca-domain}')
[ "${ca_domain}" = e2e.test ] || fail "the cluster's CA records the domain '${ca_domain}', not e2e.test"
ca_data=$(kubectl -n "${ns}" get secret "${ingress_ca}" -o jsonpath='{.data.tls\.crt}')
printf '%s' "${ca_data}" | base64 -d > "${work}/ingress-ca.crt"
ca_text=$(openssl x509 -in "${work}/ingress-ca.crt" -noout -text)
# A CA that vouches only for names in the domain and for no address cannot stand in for any other
# server its users reach, however far it is trusted.
printf '%s\n' "${ca_text}" | grep -A1 'Permitted:' | grep -qx ' *DNS:e2e.test' || fail "the cluster's CA is not constrained to e2e.test"
printf '%s\n' "${ca_text}" | grep -q 'IP:0.0.0.0/0.0.0.0' || fail "the cluster's CA may vouch for IP addresses"
printf '%s\n' "${ca_text}" | grep -q 'ASN1 OID: prime256v1' || fail "the cluster's CA's key is not ECDSA P-256"
openssl x509 -in "${work}/ingress-ca.crt" -noout -checkend $((9 * 365 * 86400)) > /dev/null || fail "the cluster's CA runs out within nine years"
marked=$(kubectl -n "${ns}" get cluster "${name}" -o json | jq '.metadata.annotations // {} | has("kubehenge.dev/certificates")')
[ "${marked}" = true ] || fail "the Cluster is not marked as having certificates"
# shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
ca_copied() { ca_copy=$(wl -n cert-manager get secret kubehenge-ingress-ca -o jsonpath='{.data.tls\.crt}' 2> /dev/null) && [ "${ca_copy}" = "${ca_data}" ]; }
until_true 300 "the cluster's CA in the workload cluster" ca_copied
until_true 600 "the default certificate issued" wl -n kube-system wait --for=condition=Ready certificate.cert-manager.io/kubehenge-ingress --timeout=10s
# default_certificate: whether the default certificate covers the domain and every name one level
# below it, with an ECDSA P-256 key.
default_certificate() {
    leaf=$(wl -n kube-system get secret kubehenge-ingress-tls -o jsonpath='{.data.tls\.crt}')
    leaf=$(printf '%s' "${leaf}" | base64 -d | openssl x509 -noout -text)
    sans=$(printf '%s\n' "${leaf}" | grep -A1 'Subject Alternative Name' | tail -n 1 | tr -d ' ' | tr ',' '\n' | LC_ALL=C sort | tr '\n' ' ')
    [ "${sans}" = 'DNS:*.e2e.test DNS:e2e.test ' ] || fail "the default certificate names ${sans}, not e2e.test and *.e2e.test"
    printf '%s\n' "${leaf}" | grep -q 'ASN1 OID: prime256v1' || fail "the default certificate's key is not ECDSA P-256"
}
default_certificate
# websecure_programmed: whether Traefik's Gateway has its HTTPS listener, terminating TLS with the
# default certificate for routes from every namespace, and has programmed it.
websecure_programmed() {
    wl -n kube-system get gateway traefik-gateway -o json 2> /dev/null | jq -e '
        any(.spec.listeners[]; .name == "websecure" and .port == 8443 and .protocol == "HTTPS" and .tls.mode == "Terminate" and
            any(.tls.certificateRefs[]; .name == "kubehenge-ingress-tls") and .allowedRoutes.namespaces.from == "All") and
        any(.status.listeners[]?; .name == "websecure" and
            any(.conditions[]; .type == "Programmed" and .status == "True") and any(.conditions[]; .type == "ResolvedRefs" and .status == "True"))' > /dev/null
}
until_true 300 "Traefik's Gateway serves HTTPS with the default certificate" websecure_programmed
new_namespace wl e2e-certs
agnhost_pod e2e-certs web netexec --http-port=8080
wl -n e2e-certs expose pod web --port=80 --target-port=8080
# Neither names a certificate: the default one serves both.
wl -n e2e-certs create ingress web --rule='web.e2e.test/*=web:80'
wl -n e2e-certs apply -f - << 'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: web
spec:
  parentRefs:
    - name: traefik-gateway
      namespace: kube-system
      sectionName: websecure
  hostnames: [gateway.e2e.test]
  rules:
    - backendRefs:
        - name: web
          port: 80
EOF
wl -n e2e-certs wait --for=condition=Ready pod/web --timeout=5m
# https_answers <CA file> <host name> <path> <pattern>: whether the API address answers the path at
# the host name over HTTPS, with a certificate the CA vouches for, matching the pattern.
https_answers() {
    https_answer=$(curl -sS --fail --max-time 5 --cacert "$1" --resolve "$2:443:${KUBEHENGE_E2E_ENDPOINT}" "https://$2$3" 2> /dev/null) || return 1
    printf '%s' "${https_answer}" | grep -qiE "$4"
}
until_true 300 "an Ingress served over HTTPS with the default certificate" https_answers "${work}/ingress-ca.crt" web.e2e.test /hostname '^web$'
until_true 300 "an HTTPRoute served over HTTPS with the default certificate" https_answers "${work}/ingress-ca.crt" gateway.e2e.test /hostname '^web$'
control_plane_use "${work}/workload" certificates

# Pebble, an ACME server made for tests, validates challenges against the DNS server it is given:
# BIND, which takes the DNS-01 records over RFC 2136 and answers for the HTTP-01 name.
step "an ACME server, and a DNS server for its challenges"
# renovate: datasource=docker depName=docker.io/internetsystemsconsortium/bind9
bind_image=docker.io/internetsystemsconsortium/bind9:9.20@sha256:66092edb53c081fa11b1fa8219e99065b0f497cb38f568ba789702a4c10e2590
# renovate: datasource=docker depName=ghcr.io/letsencrypt/pebble
pebble_image=ghcr.io/letsencrypt/pebble:2.10.1@sha256:ddf230642b1a584f519f32e347de1b05a6e4c1f6c35c1863b33effeab5f78199
# The HTTP-01 names live in the cluster's own DNS, as Services in e2e-certs whose endpoint is the API
# address, where Traefik answers on port 80; cert-manager's self-check finds them there, and Pebble
# in BIND's copy of the same records, so that nothing outside the cluster need resolve them.
http01_zone=e2e-certs.svc.cluster.local
http01_host=http01.${http01_zone}
http01_headlamp=headlamp.${http01_zone}
new_namespace wl e2e-acme
wl -n e2e-acme apply -f - << 'EOF'
apiVersion: v1
kind: Service
metadata:
  name: dns
spec:
  selector:
    app: dns
  ports:
    - name: dns
      protocol: UDP
      port: 53
      targetPort: 5353
    - name: dns-tcp
      protocol: TCP
      port: 53
      targetPort: 5353
---
apiVersion: v1
kind: Service
metadata:
  name: pebble
spec:
  selector:
    app: pebble
  ports:
    - name: acme
      port: 14000
    - name: management
      port: 15000
EOF
dns_ip=$(wl -n e2e-acme get service dns -o jsonpath='{.spec.clusterIP}')
tsig=$(head -c 32 /dev/urandom | base64 -w0)
mkdir -p "${work}/dns"
cat > "${work}/dns/named.conf" << EOF
options {
    directory "/var/cache/bind";
    pid-file none;
    listen-on port 5353 { any; };
    listen-on-v6 { none; };
    recursion no;
    dnssec-validation no;
    allow-transfer { none; };
};
controls { };
key "e2e" {
    algorithm hmac-sha256;
    secret "${tsig}";
};
zone "e2e.test" {
    type primary;
    file "/var/lib/bind/e2e.test.zone";
    allow-update { key "e2e"; };
};
zone "${http01_zone}" {
    type primary;
    file "/var/lib/bind/http01.zone";
};
EOF
cat > "${work}/dns/e2e.test.zone" << EOF
\$TTL 60
@ IN SOA ns hostmaster 1 60 60 600 60
@ IN NS ns
ns IN A ${dns_ip}
EOF
cat > "${work}/dns/http01.zone" << EOF
\$TTL 60
@ IN SOA ns hostmaster 1 60 60 600 60
@ IN NS ns
ns IN A ${dns_ip}
${http01_host}. IN A ${KUBEHENGE_E2E_ENDPOINT}
${http01_headlamp}. IN A ${KUBEHENGE_E2E_ENDPOINT}
EOF
wl -n e2e-acme create secret generic dns --from-file="${work}/dns"
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=pebble \
    -keyout "${work}/pebble.key" -out "${work}/pebble.csr" 2> /dev/null
printf 'subjectAltName = DNS:pebble.e2e-acme.svc, DNS:pebble.e2e-acme.svc.cluster.local\nextendedKeyUsage = serverAuth\n' > "${work}/pebble.cnf"
openssl x509 -req -in "${work}/pebble.csr" -CA "${work}/ca.crt" -CAkey "${work}/ca.key" -CAserial "${work}/ca.srl" \
    -days 1 -extfile "${work}/pebble.cnf" -out "${work}/pebble.crt" 2> /dev/null
wl -n e2e-acme create secret tls pebble-tls --cert="${work}/pebble.crt" --key="${work}/pebble.key"
# Both run as Pod Security's restricted level and Kyverno require: BIND as an unprivileged user on
# an unprivileged port, with its zones copied where it may write their journals.
wl -n e2e-acme apply -f - << EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: pebble
data:
  pebble.json: |
    {"pebble": {"listenAddress": "0.0.0.0:14000", "managementListenAddress": "0.0.0.0:15000",
      "certificate": "/tls/tls.crt", "privateKey": "/tls/tls.key", "httpPort": 80, "tlsPort": 443,
      "ocspResponderURL": "", "externalAccountBindingRequired": false}}
---
apiVersion: v1
kind: Pod
metadata:
  name: dns
  labels:
    app: dns
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: named
      image: ${bind_image}
      command: [/bin/sh, -c, "cp /config/*.zone /var/lib/bind/ && exec /usr/sbin/named -g -c /config/named.conf"]
      ports:
        - containerPort: 5353
          protocol: UDP
        - containerPort: 5353
          protocol: TCP
      readinessProbe:
        tcpSocket:
          port: 5353
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: config
          mountPath: /config
          readOnly: true
        - name: zones
          mountPath: /var/lib/bind
        - name: cache
          mountPath: /var/cache/bind
        - name: run
          mountPath: /run/named
  volumes:
    - name: config
      secret:
        secretName: dns
    - name: zones
      emptyDir: {}
    - name: cache
      emptyDir: {}
    - name: run
      emptyDir: {}
---
apiVersion: v1
kind: Pod
metadata:
  name: pebble
  labels:
    app: pebble
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: pebble
      image: ${pebble_image}
      args: [-config, /config/pebble.json, -dnsserver, "${dns_ip}:53"]
      env:
        # Validations at once and no nonces refused at random: the test is of the cluster, not of
        # an ACME client's patience.
        - name: PEBBLE_VA_NOSLEEP
          value: "1"
        - name: PEBBLE_WFE_NONCEREJECT
          value: "0"
      ports:
        - containerPort: 14000
        - containerPort: 15000
      readinessProbe:
        tcpSocket:
          port: 14000
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: config
          mountPath: /config
          readOnly: true
        - name: tls
          mountPath: /tls
          readOnly: true
  volumes:
    - name: config
      configMap:
        name: pebble
    - name: tls
      secret:
        secretName: pebble-tls
EOF
agnhost_pod e2e-acme client pause
wl -n e2e-acme wait --for=condition=Ready pod/dns pod/pebble pod/client --timeout=5m
# shellcheck disable=SC2310 # polled with set -e off, so a failed query is only a retry.
dns_answers() {
    soa=$(wl -n e2e-acme exec client -- dig +short "@${dns_ip}" e2e.test SOA 2>&1) && [ -n "${soa}" ] && return
    # A dig that dies says so only here: a timeout alone cannot tell a crashed client from a silent server.
    printf 'dig: %s\n' "${soa:-no answer}" >&2
    return 1
}
until_true 60 "BIND answers for e2e.test" dns_answers
# Pebble makes a new root each time it starts, which only its management interface hands out. The
# client fetches it there, verifying Pebble's own certificate.
acme_root() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed fetch is only a retry.
    wl -n e2e-acme exec -i client -- sh -c 'cat > /tmp/ca.crt && curl -sS --fail --max-time 5 --cacert /tmp/ca.crt https://pebble.e2e-acme.svc:15000/roots/0' \
        < "${work}/ca.crt" > "${work}/acme-root.crt" 2> /dev/null && grep -q 'BEGIN CERTIFICATE' "${work}/acme-root.crt"
}
until_true 120 "Pebble's root" acme_root
acme_server=https://pebble.e2e-acme.svc:14000/dir
# ingress_tls <namespace> <host name>: the cluster issuer the Ingress for the host name asks
# cert-manager for, and the Secrets it names, as JSON.
ingress_tls() {
    wl -n "$1" get ingress -o json | jq -c --arg h "$2" '[.items[] | select(any(.spec.rules[]; .host == $h)) |
        {issuer: .metadata.annotations["cert-manager.io/cluster-issuer"], secrets: [.spec.tls[]?.secretName]}]'
}

step "certificates over ACME's DNS-01 challenge, with the cluster's own DNS credential"
# The credential as the interface stores it, and the mark it leaves on the Cluster.
cluster_uid=$(kubectl -n "${ns}" get cluster "${name}" -o jsonpath='{.metadata.uid}')
stored_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kubectl -n "${ns}" apply -f - << EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${name}.dns01
  labels:
    kubehenge.dev/user-credential: dns01
    kubehenge.dev/credential-cluster: ${name}
  annotations:
    kubehenge.dev/credential-set-by: e2e@pve
    kubehenge.dev/credential-set-at: "${stored_at}"
  ownerReferences:
    - apiVersion: cluster.x-k8s.io/v1beta2
      kind: Cluster
      name: ${name}
      uid: ${cluster_uid}
type: kubehenge.dev/user-credential
stringData:
  token: "${tsig}"
EOF
kubectl -n "${ns}" annotate cluster "${name}" --overwrite "kubehenge.dev/credentials-stored-at=${stored_at}"
dns01=$(jq -cn --arg s "${acme_server}" --rawfile ca "${work}/ca.crt" --arg d "${dns_ip}:53" '{enabled: true, issuer: "acme-dns01", domain: "e2e.test",
    acme: {server: $s, caBundle: $ca}, dns01: {provider: "rfc2136", nameservers: [$d], rfc2136: {nameserver: $d, tsigKeyName: "e2e", tsigAlgorithm: "HMACSHA256"}}}')
set_variable certificates "${dns01}"
# One name one level too deep for the default certificate, and one it covers.
set_variable headlamp '{"enabled":true,"hostname":"headlamp.apps.e2e.test"}'
set_variable monitoring '{"enabled":true,"hostname":"grafana.e2e.test"}'
# shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
dns01_copied() {
    dns01_token=$(wl -n cert-manager get secret kubehenge-dns01 -o jsonpath='{.data.token}' 2> /dev/null) &&
        dns01_token=$(printf '%s' "${dns01_token}" | base64 -d) && [ "${dns01_token}" = "${tsig}" ]
}
until_true 300 "the cluster's DNS credential in the workload cluster" dns01_copied
# cert-manager looks for its records only at BIND, which alone knows e2e.test.
recursive() {
    wl -n cert-manager get deployment cert-manager -o json 2> /dev/null |
        jq -e --arg a "--dns01-recursive-nameservers=${dns_ip}:53" '.spec.template.spec.containers[0].args | index($a) and index("--dns01-recursive-nameservers-only=true")' > /dev/null
}
until_true 300 "cert-manager checks its records at BIND" recursive
wl -n cert-manager rollout status deployment/cert-manager --timeout=5m
# Issued by Pebble, the default certificate is one its root vouches for; the CA's never is.
until_true 900 "an Ingress served over HTTPS with the default certificate from the ACME server" \
    https_answers "${work}/acme-root.crt" web.e2e.test /hostname '^web$'
until_true 120 "an HTTPRoute served over HTTPS with the default certificate from the ACME server" \
    https_answers "${work}/acme-root.crt" gateway.e2e.test /hostname '^web$'
default_certificate
wl -n cert-manager get secret kubehenge-ingress-acme > /dev/null
until_true 900 "Headlamp served at a name of its own, with a certificate of its own" https_answers "${work}/acme-root.crt" headlamp.apps.e2e.test / '<html'
headlamp_tls=$(ingress_tls headlamp headlamp.apps.e2e.test)
[ "${headlamp_tls}" = '[{"issuer":"kubehenge-ingress","secrets":["headlamp-tls"]}]' ] ||
    fail "Headlamp's Ingress asks for ${headlamp_tls}, not its own certificate from kubehenge-ingress"
until_true 600 "Grafana served with the default certificate" https_answers "${work}/acme-root.crt" grafana.e2e.test /api/health '"database"'
grafana_tls=$(ingress_tls monitoring grafana.e2e.test)
printf '%s' "${grafana_tls}" | jq -e 'length == 1 and .[0].issuer == null' > /dev/null ||
    fail "Grafana's Ingress asks for a certificate of its own, which the default one makes needless: ${grafana_tls}"

step "certificates over ACME's HTTP-01 challenge, through the Gateway"
# Headless and without a selector, a Service resolves to the endpoints written for it.
for http01_service in http01 headlamp; do
    wl -n e2e-certs apply -f - << EOF
apiVersion: v1
kind: Service
metadata:
  name: ${http01_service}
spec:
  clusterIP: None
  ports:
    - name: http
      port: 80
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: ${http01_service}
  labels:
    kubernetes.io/service-name: ${http01_service}
    endpointslice.kubernetes.io/managed-by: e2e.kubehenge.dev
addressType: IPv4
ports:
  - name: http
    port: 80
    protocol: TCP
endpoints:
  - addresses: [${KUBEHENGE_E2E_ENDPOINT}]
EOF
done
# http01_resolved <name>: whether the cluster's DNS and BIND both give the name the API address.
# shellcheck disable=SC2310 # polled with set -e off, so a failed query is only a retry.
http01_resolved() {
    http01_cluster=$(wl -n e2e-acme exec client -- dig +short "$1" A 2>&1) &&
        http01_bind=$(wl -n e2e-acme exec client -- dig +short "@${dns_ip}" "$1" A 2>&1) &&
        [ "${http01_cluster}" = "${KUBEHENGE_E2E_ENDPOINT}" ] && [ "${http01_bind}" = "${KUBEHENGE_E2E_ENDPOINT}" ]
}
until_true 60 "${http01_host} resolved to the API address" http01_resolved "${http01_host}"
until_true 60 "${http01_headlamp} resolved to the API address" http01_resolved "${http01_headlamp}"
# Pebble and cert-manager's self-check fetch the challenge from a pod; any answer at all is Traefik's,
# a 404 while nothing routes the name.
# shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
http01_reached() {
    http01_status=$(wl -n e2e-acme exec client -- curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "http://${http01_host}/" 2> /dev/null) &&
        [ "${http01_status}" != 000 ]
}
until_true 60 "Traefik reached from a pod at ${http01_host} on port 80" http01_reached
http01=$(jq -cn --arg s "${acme_server}" --rawfile ca "${work}/ca.crt" '{enabled: true, issuer: "acme-http01", acme: {server: $s, caBundle: $ca}}')
set_variable certificates "${http01}"
set_variable headlamp "{\"enabled\":true,\"hostname\":\"${http01_headlamp}\"}"
set_variable monitoring '{"enabled":true}'
until_true 300 "the default certificate removed" absent wl -n kube-system get certificate.cert-manager.io/kubehenge-ingress
until_true 300 "its Secret removed with it" absent wl -n kube-system get secret/kubehenge-ingress-tls
until_true 300 "the DNS credential removed from the workload cluster" absent wl -n cert-manager get secret/kubehenge-dns01
websecure_gone() { wl -n kube-system get gateway traefik-gateway -o json | jq -e 'all(.spec.listeners[]; .name != "websecure")' > /dev/null; }
until_true 300 "the Gateway's HTTPS listener removed with the default certificate" websecure_gone
# On the HTTPS entry point alone, so that the challenge's route is the only one for the name over
# HTTP. ingress-shim gives it a certificate from the cluster's default issuer.
wl -n e2e-certs apply -f - << EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: http01
  annotations:
    kubernetes.io/tls-acme: "true"
    traefik.ingress.kubernetes.io/router.entrypoints: websecure
spec:
  tls:
    - hosts: [${http01_host}]
      secretName: http01-tls
  rules:
    - host: ${http01_host}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: web
                port:
                  number: 80
EOF
until_true 900 "an Ingress served with its own certificate over HTTP-01" https_answers "${work}/acme-root.crt" "${http01_host}" /hostname '^web$'
until_true 900 "Headlamp served with its own certificate over HTTP-01" https_answers "${work}/acme-root.crt" "${http01_headlamp}" / '<html'
headlamp_tls=$(ingress_tls headlamp "${http01_headlamp}")
[ "${headlamp_tls}" = '[{"issuer":"kubehenge-ingress","secrets":["headlamp-tls"]}]' ] ||
    fail "Headlamp's Ingress asks for ${headlamp_tls}, not its own certificate from kubehenge-ingress"

step "GitOps with Flux, then Argo CD, from a Git server in the cluster through a deploy key"
# The repository lives in the cluster itself, so that no outside service is needed. The engine
# reads it over SSH with the deploy key the controller makes; its host key is given, since the
# controller, outside the cluster, cannot reach the server to read it.
ssh-keygen -q -t ed25519 -N '' -C '' -f "${work}/git-host"
git_host=gitea.e2e-git.svc.cluster.local
known_hosts="[${git_host}]:2222 $(cut -d' ' -f1,2 "${work}/git-host.pub")"
git_password=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
new_namespace wl e2e-git
wl -n e2e-git create secret generic gitea-host-key --from-file=host="${work}/git-host"
wl -n e2e-git apply -f - << EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gitea
spec:
  selector:
    matchLabels: {app: gitea}
  template:
    metadata:
      labels: {app: gitea}
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: gitea
          image: ${gitea}
          env:
            - {name: GITEA__security__INSTALL_LOCK, value: "true"}
            - {name: GITEA__service__DISABLE_REGISTRATION, value: "true"}
            - {name: GITEA__server__SSH_SERVER_HOST_KEYS, value: /etc/gitea-host-key/host}
            - {name: GITEA__server__ROOT_URL, value: "http://${git_host}:3000/"}
          ports:
            - {name: http, containerPort: 3000}
            - {name: ssh, containerPort: 2222}
          readinessProbe:
            httpGet: {path: /api/healthz, port: http}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
          volumeMounts:
            - {name: data, mountPath: /var/lib/gitea}
            - {name: config, mountPath: /etc/gitea}
            - {name: host-key, mountPath: /etc/gitea-host-key, readOnly: true}
      volumes:
        - {name: data, emptyDir: {}}
        - {name: config, emptyDir: {}}
        - {name: host-key, secret: {secretName: gitea-host-key}}
---
apiVersion: v1
kind: Service
metadata:
  name: gitea
spec:
  selector: {app: gitea}
  ports:
    - {name: http, port: 3000, targetPort: http}
    - {name: ssh, port: 2222, targetPort: ssh}
EOF
wl -n e2e-git rollout status deployment/gitea --timeout=5m
wl -n e2e-git exec deployment/gitea -- gitea admin user create --username e2e --password "${git_password}" \
    --email e2e@example.org --admin --must-change-password=false
agnhost_pod e2e-git client pause
wl -n e2e-git wait --for=condition=Ready pod/client --timeout=5m
# git_api <method> <path> [<curl options>]: a call to the Git server's API, as its admin.
git_api() {
    git_method=$1 git_path=$2
    shift 2
    KUBECONFIG=${work}/workload kubectl -n e2e-git exec client -- curl -sS --fail --max-time 10 -u "e2e:${git_password}" \
        -H 'Content-Type: application/json' -X "${git_method}" "$@" "http://gitea:3000/api/v1/${git_path}"
}
# manifest <revision>: what the repository asks for, base64-encoded as the API takes a file.
manifest() {
    printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: e2e-gitops\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: synced\n  namespace: e2e-gitops\ndata:\n  revision: "%s"\n' "$1" | base64 -w0
}
contents=repos/e2e/clusters/contents/apps/cluster.yaml
git_api POST user/repos -d '{"name":"clusters","private":true,"auto_init":true,"default_branch":"main"}' > /dev/null
git_file=$(manifest one)
git_request=$(jq -cn --arg c "${git_file}" '{content: $c, branch: "main", message: "one"}')
git_api POST "${contents}" -d "${git_request}" > /dev/null
# push <revision>: commits the manifest at a new revision.
push() {
    git_current=$(git_api GET "${contents}")
    git_file=$(manifest "$1")
    git_request=$(printf '%s' "${git_current}" | jq -c --arg c "${git_file}" --arg m "$1" '{content: $c, sha: .sha, branch: "main", message: $m}')
    git_api PUT "${contents}" -d "${git_request}" > /dev/null
}
# synced <revision>: whether the cluster runs the manifest at that revision.
synced() {
    # shellcheck disable=SC2310 # wl wraps one kubectl call; a missing ConfigMap is only a retry.
    synced_revision=$(wl -n e2e-gitops get configmap synced -o jsonpath='{.data.revision}' 2> /dev/null) || return 1
    [ "${synced_revision}" = "$1" ]
}
gitops_value() {
    jq -cn --arg url "ssh://git@${git_host}:2222/e2e/clusters.git" --arg hosts "${known_hosts}" --arg engine "$1" --argjson on "$2" \
        '{enabled: $on, engine: $engine, auth: "deployKey", repository: {url: $url, revision: "main", path: "apps"}, knownHosts: $hosts, signIn: {clientSecret: "none"}}'
}
gitops=$(gitops_value flux true)
set_variable gitops "${gitops}"
# deploy_key: the public half of the deploy key the controller made, once it has made it.
deploy_key() {
    encoded_key=$(kubectl -n "${ns}" get secret "${name}-gitops" -o jsonpath='{.data.identity\.pub}' 2> /dev/null) || return 1
    [ -n "${encoded_key}" ] && printf '%s' "${encoded_key}" | base64 -d
}
has_deploy_key() { deploy_key > /dev/null; }
until_true 300 "the cluster's deploy key made" has_deploy_key
public_key=$(deploy_key)
# Read only, as the interface tells its users to add it.
git_request=$(jq -cn --arg k "${public_key}" '{title: "kubehenge", key: $k, read_only: true}')
git_api POST repos/e2e/clusters/keys -d "${git_request}" > /dev/null
until_true 600 "Flux installed by its operator" wl -n flux-system wait --for=condition=Ready fluxinstance/flux --timeout=10s
until_true 600 "Flux syncs the repository" synced one
push two
until_true 300 "Flux syncs a new commit" synced two
control_plane_use "${work}/workload" flux
pod_security=$(wl label --dry-run=server --overwrite namespace flux-system pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "Flux's pods do not meet Pod Security's restricted level: ${pod_security}" ;;
    *) ;;
esac

gitops=$(gitops_value argocd true)
set_variable gitops "${gitops}"
# Flux goes first and leaves what it synced running, so that the two never fight over it.
until_true 600 "Flux uninstalled" absent wl get crd kustomizations.kustomize.toolkit.fluxcd.io
until_true 600 "the Flux Operator removed" absent wl -n flux-system get deployment/flux-operator
# shellcheck disable=SC2310 # each answers the check.
absent wl -n flux-system get secret/kubehenge-gitops || fail "Flux's copy of the deploy key outlives Flux"
# shellcheck disable=SC2310 # each answers the check.
synced two || fail "what Flux synced went with it"
until_true 900 "Argo CD available" wl -n argocd wait --for=condition=Available deployment/argocd-server deployment/argocd-repo-server --timeout=10s
until_true 300 "Argo CD's application controller ready" wl -n argocd rollout status statefulset/argocd-application-controller --timeout=10s
push three
until_true 600 "Argo CD syncs a new commit" synced three
# The sync applies the ConfigMap before Argo CD records the sync's outcome on the application.
app_settled() {
    # shellcheck disable=SC2310 # wl wraps one kubectl call; a failed read is only a retry.
    app_status=$(wl -n argocd get application kubehenge -o jsonpath='{.status.sync.status} {.status.health.status}') || return 1
    [ "${app_status}" = "Synced Healthy" ]
}
until_true 120 "Argo CD's application synced and healthy" app_settled
admin=$(wl -n argocd get configmap argocd-cm -o jsonpath='{.data.admin\.enabled}')
[ "${admin}" = false ] || fail "Argo CD's built-in admin is enabled"
pod_security=$(wl label --dry-run=server --overwrite namespace argocd pod-security.kubernetes.io/enforce=restricted 2>&1)
case ${pod_security} in
    *violate*) fail "Argo CD's pods do not meet Pod Security's restricted level: ${pod_security}" ;;
    *) ;;
esac
control_plane_use "${work}/workload" argocd

# The class's minimum sizes have to carry every add-on at once.
step "every add-on at once, with nothing restarted"
found_restarts() {
    if [ "${KUBEHENGE_E2E_RESTARTS:-fail}" = report ]; then
        echo "not failing, on a host that starves its guests: $*"
    else
        fail "$*"
    fi
}
if [ -n "${server_unit}" ]; then
    # shellcheck disable=SC2310 # a failed count fails the step.
    servers_after=$(server_restarts) || fail "cannot read ${server_unit}'s restarts on the control planes"
    [ "${servers_after}" = "${servers_before}" ] ||
        found_restarts "${server_unit} restarted $((servers_after - servers_before)) times on the control planes while the add-ons were installed"
fi
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
        if [ "${distribution}" = talos ]; then
            pressure_address=$(machine_address "${m}")
            # shellcheck disable=SC2310 # a node that cannot be read is said so, not fatal here.
            talos_read "${pressure_address}" /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io ||
                echo "  cannot read it"
        else
            # shellcheck disable=SC2310 # a node that cannot be read is said so, not fatal here.
            on_machines "${m}" "grep -H . /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io" ||
                echo "  cannot read it"
        fi
    done
    found_restarts "containers restarted while the add-ons were installed (pod, restarts): ${restarted}"
fi
pod_memory "${work}/workload"
echo "CPU requested with every add-on on:"
pod_cpu "${work}/workload"

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

if [ "${distribution}" = talos ]; then
    # With every add-on on, as a cluster in use is upgraded: the controller drains each node, has it
    # reboot into the new release and lets it back, the control plane first, on the same machines.
    step "Talos upgraded in place to ${talos_to}, node by node, with every add-on on"
    machine_names() { kubectl -n "${ns}" get machines -l "cluster.x-k8s.io/cluster-name=${name}" -o json | jq -c '[.items[].metadata.name] | sort'; }
    talos_machines_before=$(machine_names)
    talos_since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    talos_cordoned=' '
    set_variable talosVersion "\"${talos_to}\""
    # talos_upgraded: whether every node runs the release, none being upgraded any more; notes each
    # node seen cordoned on the way, and fails the test at a failed upgrade.
    talos_upgraded() {
        # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
        talos_cluster=$(kubectl -n "${ns}" get "cluster/${name}" -o json) || return 1
        talos_failed=$(printf '%s' "${talos_cluster}" | jq -r '.metadata.annotations["kubehenge.dev/talos-upgrade-failed"] // ""')
        [ -z "${talos_failed}" ] || fail "a node's Talos upgrade failed: ${talos_failed}"
        # The API is down while the one control plane reboots.
        # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
        if talos_nodes=$(wl get nodes -o json 2> /dev/null); then
            for talos_node in $(printf '%s' "${talos_nodes}" | jq -r '.items[] | select(.spec.unschedulable == true) | .metadata.name'); do
                case ${talos_cordoned} in
                    *" ${talos_node} "*) ;;
                    *) talos_cordoned="${talos_cordoned}${talos_node} " ;;
                esac
            done
        fi
        printf '%s' "${talos_cluster}" | jq -e '.metadata.annotations["kubehenge.dev/talos-upgrading"] == null' > /dev/null || return 1
        machines_on_talos "${talos_to}"
    }
    until_true 2700 "every node on Talos ${talos_to}" talos_upgraded
    talos_machines_after=$(machine_names)
    [ "${talos_machines_after}" = "${talos_machines_before}" ] ||
        fail "the machines ${talos_machines_before} were replaced by ${talos_machines_after}, not upgraded in place"
    talos_last=$(talos_annotation talos-upgraded)
    case ${talos_last} in
        *" to ${talos_to} at "*) ;;
        *) fail "kubehenge.dev/talos-upgraded records '${talos_last}', not an upgrade to ${talos_to}" ;;
    esac
    # A node is cordoned only while it is upgraded, which a poll can miss; its kubelet tells of it
    # too.
    not_schedulable=$(wl get events -A --field-selector reason=NodeNotSchedulable -o json |
        jq -r --arg s "${talos_since}" '[.items[] | select((.lastTimestamp // .eventTime // "") >= $s) | .involvedObject.name] | unique | join(" ")')
    for talos_node in $(wl get nodes -o jsonpath='{.items[*].metadata.name}'); do
        case " ${talos_cordoned} ${not_schedulable} " in
            *" ${talos_node} "*) ;;
            *) fail "node ${talos_node} was upgraded without being drained" ;;
        esac
    done
    cordoned=$(wl get nodes -o json | jq -r '[.items[] | select(.spec.unschedulable == true) | .metadata.name] | join(" ")')
    [ -z "${cordoned}" ] || fail "nodes left cordoned after their upgrade: ${cordoned}"
    for address in $(talos_addresses); do
        # shellcheck disable=SC2310 # a node that does not answer fails the step.
        release=$(talos_release "${address}") || fail "the Talos API of ${address} does not answer"
        [ "${release}" = "${talos_to}" ] || fail "the Talos API of ${address} reports '${release}', not ${talos_to}"
    done
    # Machines made before the next Kubernetes upgrade still boot the release before, and the
    # controller upgrades them in turn.
    talos_image=$(talos_annotation talos-image)
    [ "${talos_image}" = "${talos_from}" ] || fail "new machines boot Talos '${talos_image}' before a Kubernetes upgrade, not ${talos_from}"
    # releases_deployed: every add-on's Helm release is deployed and ready.
    releases_deployed() {
        # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
        proxies=$(kubectl -n "${ns}" get helmreleaseproxies -l "cluster.x-k8s.io/cluster-name=${name}" -o json) || return 1
        printf '%s' "${proxies}" | jq -e '.items | length > 0 and all(.status.status == "deployed" and
            any(.status.conditions[]?; .type == "Ready" and .status == "True"))' > /dev/null
    }
    # workloads_ready: every Deployment, StatefulSet and DaemonSet in the cluster has its pods ready.
    workloads_ready() {
        # shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
        workloads=$(wl get deployments,statefulsets,daemonsets -A -o json) || return 1
        printf '%s' "${workloads}" | jq -e '.items | all(if .kind == "DaemonSet"
            then (.status.numberReady // 0) == .status.desiredNumberScheduled
            else (.status.readyReplicas // 0) == (.spec.replicas // 1) end)' > /dev/null
    }
    until_true 900 "every add-on's release deployed after the upgrade" releases_deployed
    until_true 900 "every workload ready after the upgrade" workloads_ready
    until_true 600 "monitoring reaches every component after the upgrade" all_up
    wl get nodes -o wide
fi

step "every add-on switched off at once"
# Removing them all at once is the control planes' heaviest moment.
control_plane_record "${work}/workload" &
recorder_pid=$!
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
set_variable certificates '{"enabled":false}'
gitops=$(gitops_value argocd false)
set_variable gitops "${gitops}"
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
kill "${recorder_pid}" 2> /dev/null || true
recorder_pid=
kill "${s3_pid}"
s3_pid=
# removed <kubectl get...>: whether the object is gone, or its kind with it, as cert-manager's own
# kinds go once nothing needs cert-manager.
removed() {
    # shellcheck disable=SC2310 # the absence is the answer looked for.
    absent "$@" && return
    removed_out=$("$@" -o name 2>&1) && return 1
    case ${removed_out} in
        *"the server doesn't have a resource type"*) ;;
        *) return 1 ;;
    esac
}
until_true 300 "the certificates' issuer removed" removed wl get clusterissuers.cert-manager.io/kubehenge-ingress
until_true 300 "the default certificate removed" removed wl -n kube-system get certificates.cert-manager.io/kubehenge-ingress
until_true 300 "the DNS credential removed from the workload cluster" absent wl -n cert-manager get secret/kubehenge-dns01
# shellcheck disable=SC2310 # polled with set -e off, so a failed read is only a retry.
unmarked() {
    marked=$(kubectl -n "${ns}" get cluster "${name}" -o json | jq '.metadata.annotations // {} | has("kubehenge.dev/certificates")') &&
        [ "${marked}" = false ]
}
until_true 300 "the Cluster no longer marked as having certificates" unmarked
until_true 900 "cert-manager removed, with nothing left that needs it" absent wl -n cert-manager get deployment/cert-manager
# The CA stays while the cluster lives, so that certificates switched on again keep it.
kubectl -n "${ns}" get secret "${ingress_ca}" > /dev/null || fail "the cluster's CA went with its certificates"
wl delete namespace e2e-certs e2e-acme --wait --timeout=5m
until_true 600 "Argo CD removed" absent wl -n argocd get deployment/argocd-server
until_true 300 "Argo CD's CRDs removed" absent wl get crd applications.argoproj.io
# shellcheck disable=SC2310 # each answers the check.
absent wl -n argocd get secret/kubehenge-repository || fail "Argo CD's copy of the deploy key outlives Argo CD"
# shellcheck disable=SC2310 # each answers the check.
synced three || fail "what Argo CD synced went with it"
wl delete namespace e2e-gitops e2e-git --wait --timeout=5m

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
if [ "${distribution}" = talos ]; then
    # A worker the scaling made booted the release before, and is upgraded in place first.
    until_true 2700 "every node on Talos ${talos_to} before the upgrade" talos_upgraded
    talos_last=$(talos_annotation talos-upgraded)
fi
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
if [ "${distribution}" = talos ]; then
    # The upgrade replaced every machine, so its new ones were cloned from the release the nodes
    # run and need no upgrade in place.
    talos_image=$(talos_annotation talos-image)
    [ "${talos_image}" = "${talos_to}" ] || fail "the upgrade's machines boot Talos '${talos_image}', not ${talos_to}"
    until_true 600 "every machine on Talos ${talos_to} after the upgrade" machines_on_talos "${talos_to}"
    talos_now=$(talos_annotation talos-upgraded)
    [ "${talos_now}" = "${talos_last}" ] || fail "a machine the upgrade made was upgraded in place: ${talos_now}"
fi
# The new control-plane nodes have to carry the authentication configuration too.
check_sign_in
# The distribution's own Gateway API chart takes over what its Traefik CRD chart shipped before,
# and Kubehenge's policy, without Kubehenge applying anything beside it.
if [ -n "${gateway_api_chart}" ]; then
    until_true 300 "${gateway_api_chart} holds the Gateway API after the upgrade" gateway_api_chart_holds
fi
until_true 300 "the Gateway API's objects after the upgrade, each held once" gateway_api_held
until_true 300 "Traefik's Gateway is programmed after the upgrade" gateway_programmed

# Talos is its own OS, with no shell for the guest agent to run the check in; its nodes' release is
# checked through the Talos API above.
if [ "${distribution}" != talos ]; then
    step "every node runs ${node_os} with SELinux enforcing, and nothing it ran was denied"
    # After every add-on, the upgrade and the restore, so that a denial any of them met shows here.
    case ${node_os} in
        almalinux-*) os_id=almalinux ;;
        rocky-*) os_id=rocky ;;
        leap-*) os_id=opensuse-leap ;;
        *) fail "no operating system ID known for ${node_os}" ;;
    esac
    selinux_check="
        . /etc/os-release
        case \"\${ID} \${VERSION_ID}\" in
            '${os_id} ${node_os#*-}' | '${os_id} ${node_os#*-}'.*) ;;
            *) echo \"runs \${ID} \${VERSION_ID}\"; exit 1 ;;
        esac
        [ \"\$(getenforce)\" = Enforcing ] || { echo \"SELinux is \$(getenforce)\"; exit 1; }
        if touch /usr/.kubehenge-e2e 2> /dev/null; then rm -f /usr/.kubehenge-e2e; echo '/usr is writable'; exit 1; fi
        denials=\$({ journalctl -k -b --no-pager -q; cat /var/log/audit/audit.log 2>/dev/null; } | grep 'avc: *denied' || true)
        [ -z \"\${denials}\" ] || { printf '%s\n' \"\${denials}\" | tail -n 20; exit 1; }"
    for m in $(kubectl -n "${ns}" get machines -o jsonpath='{.items[*].metadata.name}'); do
        # shellcheck disable=SC2310 # its output says why, which the failure reports.
        if ! out=$(on_machines "${m}" "${selinux_check}"); then
            printf '%s\n' "${out}" >&2
            fail "${m} does not enforce SELinux on ${node_os}, or something it ran was denied"
        fi
    done
fi

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
creator -n "${ns}" delete "cluster/${name}" --wait --timeout=30m
# A failed request is not an empty list.
# shellcheck disable=SC2310 # polled; a failed request leaves the pool counted as there.
pool_removed() { pools=$(pve_get pools) && ! printf '%s' "${pools}" | jq -e --arg p "${pool}" '.data[] | select(.poolid == $p)' > /dev/null; }
until_true 300 "pool ${pool} removed" pool_removed
users=$(pve_get access/users)
printf '%s' "${users}" | jq -e --arg u "${user}" '[.data[] | select(.userid == $u)] | length == 0' > /dev/null || fail "user ${user} is still there"
vms=$(pve_get "cluster/resources?type=vm")
printf '%s' "${vms}" | jq -e --argjson o "${owner}" --arg n "${name}" '[.data[] | select(.vmid == $o or (.name // "" | startswith($n + "-")))] | length == 0' > /dev/null ||
    fail "VMs of the cluster are still there"
if [ -n "${KUBEHENGE_E2E_NAMESPACE:-}" ]; then
    # The namespace is the appliance's and stays; nothing of the cluster may, once the garbage
    # collector has taken what the Cluster owned, but its backup keys: they stay beside it, so that
    # a cluster made again under its name reads its backups.
    keys=secret/${name}-backup-keys
    # left_of_cluster: what of the cluster is still in its namespace, its backup keys aside.
    left_of_cluster() {
        objects=$(kubectl -n "${ns}" get secrets,configmaps,machines -l "cluster.x-k8s.io/cluster-name=${name}" -o name) || return 1
        printf '%s\n' "${objects}" | grep -vxF -e "${keys}" -e '' || true
    }
    # A failed request is not an empty list.
    # shellcheck disable=SC2310 # polled; a failed request counts as something left.
    nothing_left() { left=$(left_of_cluster) && [ -z "${left}" ]; }
    # shellcheck disable=SC2310 # until_true fails on its own; the subshell lets the leftovers be named.
    if ! (until_true 300 "nothing of ${name} left in ${ns}" nothing_left); then
        left=$(left_of_cluster) || left='(no answer)'
        fail "still in ${ns}: ${left}"
    fi
    kubectl -n "${ns}" get "${keys}" > /dev/null || fail "${name}'s backup keys went with it"
    # A test cluster's keys guard backups of nothing, and the namespace outlives the test.
    kubectl -n "${ns}" delete "${keys}" > /dev/null
elif ! kubectl delete namespace "${ns}" --wait --timeout=5m; then
    # Its conditions say what is left in it and whose finalizers hold that.
    left=$(kubectl get namespace "${ns}" -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.message}; {end}' || true)
    fail "namespace ${ns} is still there: ${left}"
fi
if [ -n "${KUBEHENGE_E2E_NAMESPACE:-}" ]; then
    # What was kept beside the cluster goes with it, Cluster API's Secrets and the controller's,
    # which carry no label of the cluster. A failed request is not an empty list.
    secrets_gone() {
        secrets=$(kubectl -n "${ns}" get secrets -o json) || return 1
        printf '%s' "${secrets}" | jq -e --arg n "${name}" '[.items[] | select(.metadata.name | startswith($n + "-"))] | length == 0' > /dev/null
    }
    until_true 300 "the cluster's Secrets in ${ns} removed" secrets_gone
fi

# Another test may be using the class on the same management cluster.
if kubectl get clusters --all-namespaces -o json | jq -e --arg c "${class}" '[.items[] | select(.spec.topology.classRef.name == $c)] | length == 0' > /dev/null; then
    step "${class}'s providers stop with its last cluster"
    until_true 300 "${class}'s providers idle" providers_idle "${class}"
fi

printf '\nPASS\n'
