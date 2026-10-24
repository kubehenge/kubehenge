#!/bin/sh
# End-to-end test of the cluster autoscaler on a Proxmox VE host, against a running management
# cluster: a one-node k3s cluster with one pool autoscaled from 0 to 2 workers. A pod that waits for
# room brings a worker up from none, a second brings a second, and with both gone the pool goes back
# to none; its control plane stays kept for the control plane throughout. Switching autoscaling off
# leaves nothing of the autoscaler in either cluster and the pool at a fixed number again; nothing
# of the cluster is left afterwards. It reports the most VMs it saw at once and the autoscaler's
# memory.
#
#   test/e2e/autoscaler.sh
#
# KUBECONFIG points at the management cluster. KUBEHENGE_PROXMOX_URL and KUBEHENGE_PROXMOX_TOKEN
# reach Proxmox VE with rights to inspect everything; KUBEHENGE_PROXMOX_CA is the API's CA if the
# system does not trust it. Network and storage, as for workload-cluster.sh: KUBEHENGE_E2E_BRIDGE,
# KUBEHENGE_E2E_PREFIX, KUBEHENGE_E2E_GATEWAY, KUBEHENGE_E2E_DNS, KUBEHENGE_E2E_STORAGE; the
# cluster's node addresses, four, and virtual IP: KUBEHENGE_E2E_ADDRESSES, KUBEHENGE_E2E_ENDPOINT.
# KUBEHENGE_E2E_VERSION is the k3s version. KUBEHENGE_E2E_CONTROL_PLANE_CORES and _MEMORY (MiB)
# size the control plane, by default 2 cores and 4096 MiB; each worker has 2 cores and 2048 MiB.
set -eu

: "${KUBECONFIG:?KUBECONFIG must point at the management cluster}"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}"
: "${KUBEHENGE_E2E_BRIDGE:?}" "${KUBEHENGE_E2E_PREFIX:?}" "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_STORAGE:?}"
: "${KUBEHENGE_E2E_ADDRESSES:?}" "${KUBEHENGE_E2E_ENDPOINT:?}" "${KUBEHENGE_E2E_VERSION:?}"

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

work=$(mktemp -d)
ns=e2e-autoscaler-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
watcher=
cleanup() {
    status=$?
    [ -z "${watcher}" ] || kill "${watcher}" 2> /dev/null || true
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
workload() { KUBECONFIG="${work}/web" kubectl "$@"; }

# Where the controller keeps the autoscaler of cluster ${ns}/web, under one name for its account
# and Deployment: the namespace and name with a hash of the two. The autoscaler identity's token
# that it mounts is beside them, under that name and the identity's.
autoscalers=kubehenge-autoscaler
name="${ns}-web-$(printf '%s/web' "${ns}" | sha256sum | cut -c 1-8)"
token="${name}-autoscaler"
min_size=cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size
max_size=cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size

# vms: how many VMs the cluster's Proxmox VE pool holds.
vms() { pve_get "cluster/resources?type=vm" | jq --arg p "/${ns}." '[.data[] | select(.pool // "" | contains($p))] | length'; }
# The most VMs at once, counted every 15 seconds from the first worker on.
watch_vms() {
    peak=0
    while :; do
        # shellcheck disable=SC2310 # a failed count is only a missed sample.
        n=$(vms 2> /dev/null || true)
        case ${n} in
            '' | *[!0-9]*) n=0 ;;
            *) ;;
        esac
        if [ "${n}" -gt "${peak}" ]; then
            peak=${n}
            printf '%s\n' "${peak}" > "${work}/peak"
        fi
        sleep 15
    done
}
# pool: the pool's MachineDeployment, as JSON.
pool() { kubectl -n "${ns}" get machinedeployments -l cluster.x-k8s.io/cluster-name=web -o json | jq -c '.items[0]'; }
pool_asks() { pool | jq -e --argjson n "$1" '.spec.replicas == $n' > /dev/null; }
workers_ready() {
    n=$(workload get nodes -l node.cluster.x-k8s.io/pool=autoscaled -o json |
        jq '[.items[] | select(.status.conditions[] | select(.type == "Ready" and .status == "True"))] | length')
    [ "${n}" = "$1" ]
}
running_pods() {
    n=$(workload -n e2e-autoscaler get pods -l app=load --field-selector=status.phase=Running -o json |
        jq '[.items[].spec.nodeName] | unique | length')
    [ "${n}" = "$1" ]
}
cp_tainted() {
    workload get nodes -l node-role.kubernetes.io/control-plane -o json |
        jq -e 'all(.items[]; [.spec.taints[]? | select(.key == "node-role.kubernetes.io/control-plane" and .effect == "NoSchedule")] | length == 1)' > /dev/null
}
# shellcheck disable=SC2310 # the failure is the answer looked for.
cp_untainted() { ! cp_tainted; }
policy_names() {
    kubectl -n "${autoscalers}" get configmap kubehenge-autoscaler -o json | jq -e --arg k "${name}" '.data // {} | has($k)' > /dev/null
}
# shellcheck disable=SC2310 # the failure is the answer looked for.
policy_forgot() { ! policy_names; }

step "a k3s cluster with one pool the autoscaler keeps between 0 and 2 workers"
kubectl create namespace "${ns}"
addresses=$(json_list "${KUBEHENGE_E2E_ADDRESSES}")
dns_servers=$(json_list "${KUBEHENGE_E2E_DNS}")
kubectl apply -f - << EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: web
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
          metadata:
            labels:
              node.cluster.x-k8s.io/pool: autoscaled
            annotations:
              ${min_size}: "0"
              ${max_size}: "2"
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
        value: {cores: ${KUBEHENGE_E2E_CONTROL_PLANE_CORES:-2}, memoryMiB: ${KUBEHENGE_E2E_CONTROL_PLANE_MEMORY:-4096}}
      - name: workerMachine
        value: {cores: 2, memoryMiB: 2048, diskGiB: 20}
      - name: autoscaler
        value: {scaleDownUnneededMinutes: 1, scaleDownDelayAfterAddMinutes: 1}
EOF
kubectl -n "${ns}" wait --for=condition=Available cluster/web --timeout=40m
kubectl -n "${ns}" get secret web-kubeconfig -o jsonpath='{.data.value}' | base64 -d > "${work}/web"
until_true 600 "the autoscaler's Deployment" kubectl -n "${autoscalers}" get "deployment/${name}" -o name
kubectl -n "${autoscalers}" rollout status "deployment/${name}" --timeout=10m
kubectl -n "${autoscalers}" get "deployment/${name}" -o json |
    jq -e '.spec.template.spec.containers[0].image | test("^registry.k8s.io/autoscaling/cluster-autoscaler:v[0-9.]+@sha256:")' > /dev/null ||
    fail "the autoscaler does not run a pinned release"
until_true 300 "the policy names the autoscaler" policy_names
capacity() { pool | jq -e '.metadata.annotations | has("capacity.cluster-autoscaler.kubernetes.io/memory") and .["capacity.cluster-autoscaler.kubernetes.io/labels"] == "node.cluster.x-k8s.io/pool=autoscaled"' > /dev/null; }
until_true 300 "the pool's capacity told to the autoscaler" capacity
# shellcheck disable=SC2310 # pool_asks and cp_tainted end in the one jq whose status is tested.
pool_asks 0 || fail "the pool asks for workers before anything waits"
# shellcheck disable=SC2310
cp_tainted || fail "the control plane takes workloads while the pool may grow"

step "a pod waiting for room brings a worker up from none"
watch_vms &
watcher=$!
workload create namespace e2e-autoscaler
# One per worker: two of them take more memory than a worker of 2048 MiB offers.
workload -n e2e-autoscaler apply -f - << EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: load
spec:
  replicas: 1
  selector:
    matchLabels: {app: load}
  template:
    metadata:
      labels: {app: load}
    spec:
      nodeSelector:
        node.cluster.x-k8s.io/pool: autoscaled
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: pause
          image: ${pause_image}
          resources:
            requests: {cpu: 100m, memory: 1Gi}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
EOF
until_true 600 "the autoscaler asks for a worker" pool_asks 1
until_true 1800 "a worker ready" workers_ready 1
until_true 600 "the pod running on it" running_pods 1

step "a second pod brings a second worker"
workload -n e2e-autoscaler scale deployment/load --replicas=2
until_true 600 "the autoscaler asks for a second worker" pool_asks 2
until_true 1800 "two workers ready" workers_ready 2
until_true 600 "the pods running on both" running_pods 2
# shellcheck disable=SC2310 # cp_tainted ends in the one jq whose status is tested.
cp_tainted || fail "the control plane takes workloads while the pool has workers"
echo "the autoscaler's memory:"
for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    kubectl get --raw "/api/v1/nodes/${node}/proxy/stats/summary" |
        jq -r --arg ns "${autoscalers}" --arg n "${name}" '.pods[] | select(.podRef.namespace == $ns and (.podRef.name | startswith($n))) |
            "  \(.podRef.name): \(.memory.workingSetBytes / 1048576 | floor) MiB"'
done

step "with the pods gone the pool goes back to none"
workload -n e2e-autoscaler delete deployment/load --wait --timeout=5m
until_true 1200 "the autoscaler gives the workers up" pool_asks 0
until_true 1200 "the workers gone" workers_ready 0
gone() {
    kubectl -n "${ns}" get machines -l cluster.x-k8s.io/cluster-name=web,cluster.x-k8s.io/deployment-name -o json |
        jq -e '.items | length == 0' > /dev/null
}
until_true 900 "their machines gone" gone
kill "${watcher}"
watcher=
peak=0
[ -f "${work}/peak" ] && read -r peak < "${work}/peak"
printf 'the most VMs at once: %s\n' "${peak}"

step "switching autoscaling off leaves nothing of the autoscaler"
workers=$(kubectl -n "${ns}" get cluster/web -o json | jq -c --arg a "${min_size}" --arg b "${max_size}" \
    '.spec.topology.workers.machineDeployments | map(.replicas = 0 | del(.metadata.annotations[$a], .metadata.annotations[$b]))')
kubectl -n "${ns}" patch cluster/web --type=merge -p "{\"spec\":{\"topology\":{\"workers\":{\"machineDeployments\":${workers}}}}}"
until_true 300 "the autoscaler's Deployment removed" absent kubectl -n "${autoscalers}" get "deployment/${name}"
until_true 300 "its identity's token removed" absent kubectl -n "${autoscalers}" get "secret/${token}"
until_true 300 "its account removed" absent kubectl -n "${autoscalers}" get "serviceaccount/${name}"
until_true 300 "its rights removed" absent kubectl -n "${ns}" get rolebinding/kubehenge-autoscaler-web
until_true 300 "the policy forgets it" policy_forgot
until_true 300 "its account in the cluster removed" absent workload -n kube-system get serviceaccount/kubehenge-autoscaler
until_true 300 "its rights in the cluster removed" absent workload get clusterrole/kubehenge-autoscaler
until_true 300 "its status removed" absent workload -n kube-system get configmap/cluster-autoscaler-status
uncapacitated() { pool | jq -e '[.metadata.annotations // {} | keys[] | select(startswith("capacity.cluster-autoscaler.kubernetes.io/"))] | length == 0' > /dev/null; }
until_true 300 "the pool's capacity taken off" uncapacitated
# shellcheck disable=SC2310 # pool_asks ends in the one jq whose status is tested.
pool_asks 0 || fail "the pool asks for workers, not the fixed 0"
until_true 300 "the control plane takes workloads again, without workers" cp_untainted

step "delete the cluster"
kubectl -n "${ns}" delete cluster/web --wait --timeout=30m
left=$(vms)
[ "${left}" = 0 ] || fail "VMs of the cluster are still there"
pools=$(pve_get pools)
printf '%s' "${pools}" | jq -e --arg p "/${ns}." '[.data[] | select(.poolid | contains($p))] | length == 0' > /dev/null ||
    fail "the cluster's pool is still there"
kubectl delete namespace "${ns}" --wait --timeout=5m

printf '\nPASS\n'
