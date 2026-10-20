#!/bin/sh
# Shared steps of the management-cluster end-to-end tests. Sourced after `set -eu`, with $dir set to
# the root configuration under test. TF_VAR_proxmox_api_token must be set; TOFU selects the binary;
# KUBEHENGE_PROXMOX_CA is the API's CA if the system does not trust it.

: "${dir:?set dir to the root configuration before sourcing lib.sh}"
: "${TF_VAR_proxmox_api_token:?TF_VAR_proxmox_api_token must be set}"
tofu=${TOFU:-tofu}
curl_ca=
[ -n "${KUBEHENGE_PROXMOX_CA:-}" ] && curl_ca="--cacert ${KUBEHENGE_PROXMOX_CA}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export KUBECONFIG="${work}/kubeconfig"
provider_kinds=coreproviders,bootstrapproviders,controlplaneproviders,infrastructureproviders,ipamproviders,addonproviders

# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

# The root module in ${dir} calls this checkout's module, whose lock checks the providers: OpenTofu
# reads only the root module's.
apply_management_cluster() {
    cp "$(dirname "$0")/../../management-cluster/.terraform.lock.hcl" "${dir}/"
    "${tofu}" -chdir="${dir}" init -input=false -lockfile=readonly > /dev/null
    "${tofu}" -chdir="${dir}" apply -input=false -auto-approve
    "${tofu}" -chdir="${dir}" output -raw kubeconfig > "${KUBECONFIG}"
    "${tofu}" -chdir="${dir}" output -json proxmox_objects > "${work}/objects.json"
}

check_nodes() {
    kubectl wait --for=condition=Ready nodes --all --timeout=10m
    kubectl get nodes -o wide
}

check_providers() {
    kubectl wait --for=condition=Ready "${provider_kinds}" --all --all-namespaces --timeout=15m
    kubectl get "${provider_kinds}" --all-namespaces
    providers=$(kubectl get "${provider_kinds}" --all-namespaces -o name)
    for p in cluster-api kubeadm rke2 k3s proxmox in-cluster helm; do
        printf '%s\n' "${providers}" | grep -q "/${p}\$" || fail "provider ${p} is missing"
    done
    kubectl get deployments --all-namespaces -l cluster.x-k8s.io/provider \
        -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' |
        while read -r ns name; do
            kubectl -n "${ns}" rollout status "deployment/${name}" --timeout=10m
        done
}

# installed_version <provider name>: the version the operator reports as installed.
installed_version() {
    kubectl get "${provider_kinds}" --all-namespaces -o json |
        jq -r --arg n "$1" '.items[] | select(.metadata.name == $n) | .status.installedVersion' | head -n 1
}

check_topology_and_tls() {
    kubectl get crd clusterclasses.cluster.x-k8s.io > /dev/null
    kubectl get deployment --all-namespaces -l cluster.x-k8s.io/provider=infrastructure-proxmox \
        -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' | grep -q -- '--proxmox-insecure=false' ||
        fail 'capmox does not verify the Proxmox VE certificate'
    for provider in cluster-api control-plane-kubeadm infrastructure-proxmox; do
        kubectl get deployment --all-namespaces -l "cluster.x-k8s.io/provider=${provider}" \
            -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' | grep -q 'ClusterTopology=true' ||
            fail "ClusterTopology is not enabled in ${provider}"
    done
}

check_kubehenge() {
    kubectl -n kubehenge-system rollout status deployment/kubehenge-controller --timeout=10m
    # Cluster API reached the controller's lifecycle hook and knows what it serves.
    kubectl wait --for=condition=Discovered extensionconfig/kubehenge --timeout=5m
    for hook in BeforeClusterCreate BeforeClusterUpgrade BeforeClusterDelete; do
        kubectl get extensionconfig kubehenge -o json |
            jq -e --arg h "${hook}" '[.status.handlers[] | select(.requestHook.hook == $h)] | length == 1' > /dev/null ||
            fail "the controller does not serve ${hook}"
    done
    for class in kubehenge-rke2 kubehenge-k3s kubehenge-kubeadm; do
        kubectl -n kubehenge-system wait --for=condition=VariablesReady "clusterclass/${class}" --timeout=5m
        # No cluster uses a class yet, so its distribution's controllers do not run, and its
        # control-plane and bootstrap templates wait in their ConfigMap.
        until_true 300 "${class}'s providers idle" providers_idle "${class}"
        templates=$(kubectl -n kubehenge-system get configmap "${class}" -o jsonpath='{.data.templates\.yaml}')
        # grep -c exits non-zero when it counts none, which the comparison reports.
        kinds=$(printf '%s\n' "${templates}" | grep -c '^kind:' || true)
        [ "${kinds}" = 2 ] || fail "${class}'s ConfigMap does not hold its control-plane and bootstrap templates"
    done
}

check_capmox_privileges() {
    creds=$(kubectl get secret --all-namespaces -l cluster.x-k8s.io/provider=infrastructure-proxmox -o json |
        jq -c '.items[] | select(.metadata.name == "capmox-manager-credentials") | .data | map_values(@base64d)')
    [ -n "${creds}" ] || fail 'capmox has no credentials secret'
    capmox_url=$(printf '%s' "${creds}" | jq -r .url)
    [ -n "${capmox_url}" ] || fail 'capmox has no Proxmox VE URL'
    capmox_token=$(printf '%s' "${creds}" | jq -r .token)
    capmox_secret=$(printf '%s' "${creds}" | jq -r .secret)
    capmox_auth="Authorization: PVEAPIToken=${capmox_token}=${capmox_secret}"
    # shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
    capmox_get() { curl -sS --fail ${curl_ca} -H "${capmox_auth}" "${capmox_url}/api2/json/$1"; }
    nodes=$(capmox_get nodes | jq '.data | length')
    [ "${nodes}" -ge 1 ] || fail 'capmox cannot see the Proxmox VE nodes'
    # What capmox does: create VMs in the workload pool and clone from the template pool.
    workload_pool=$(jq -r .workload_pool "${work}/objects.json")
    template_pool=$(jq -r .template_pool "${work}/objects.json")
    capmox_get "access/permissions?path=/pool/${workload_pool}" |
        jq -e '[.data[] | has("VM.Allocate") and has("VM.Config.Disk")] | any' > /dev/null ||
        fail 'capmox cannot create VMs in the workload pool'
    capmox_get "access/permissions?path=/pool/${template_pool}" |
        jq -e '[.data[] | has("VM.Clone")] | any' > /dev/null ||
        fail 'capmox cannot clone templates'
    users=$(capmox_get access/users | jq '.data | length')
    [ "${users}" = 0 ] || fail 'capmox can see Proxmox VE users'
    for vmid in $(jq -r '.vm_ids[]' "${work}/objects.json"); do
        capmox_get cluster/resources?type=vm | jq -e --argjson id "${vmid}" '[.data[] | select(.vmid == $id)] | length == 0' > /dev/null ||
            fail "capmox can see management VM ${vmid}"
    done
}

destroy_and_check_nothing_left() {
    "${tofu}" -chdir="${dir}" destroy -input=false -auto-approve

    step "nothing left on Proxmox VE"
    endpoint=$(jq -r .endpoint "${work}/objects.json")
    # shellcheck disable=SC2086 # curl_ca is empty or one option and its argument.
    pve() { curl -sS --fail ${curl_ca} -H "Authorization: PVEAPIToken=${TF_VAR_proxmox_api_token}" "${endpoint}/api2/json/$1"; }
    for vmid in $(jq -r '.vm_ids[]' "${work}/objects.json"); do
        pve cluster/resources?type=vm | jq -e --argjson id "${vmid}" '[.data[] | select(.vmid == $id)] | length == 0' > /dev/null ||
            fail "VM ${vmid} is still there"
    done
    for pool in $(jq -r '.pools[]' "${work}/objects.json"); do
        pve pools | jq -e --arg p "${pool}" '[.data[] | select(.poolid == $p)] | length == 0' > /dev/null || fail "pool ${pool} is still there"
    done
    for user in $(jq -r '.users[]' "${work}/objects.json"); do
        pve access/users | jq -e --arg u "${user}" '[.data[] | select(.userid == $u)] | length == 0' > /dev/null || fail "user ${user} is still there"
    done
    for group in $(jq -r '.groups[]' "${work}/objects.json"); do
        pve access/groups | jq -e --arg g "${group}" '[.data[] | select(.groupid == $g)] | length == 0' > /dev/null || fail "group ${group} is still there"
    done
    for role in $(jq -r '.roles[]' "${work}/objects.json"); do
        pve access/roles | jq -e --arg r "${role}" '[.data[] | select(.roleid == $r)] | length == 0' > /dev/null || fail "role ${role} is still there"
    done
    for image in $(jq -r '.images | to_entries[] | "\(.key)=\(.value | split(":")[0])"' "${work}/objects.json"); do
        node=${image%%=*} storage=${image#*=}
        pve "nodes/${node}/storage/${storage}/content?content=import" |
            jq -e '[.data[] | select(.volid | test(":import/talos-"))] | length == 0' > /dev/null ||
            fail "a Talos image is still on storage ${storage} of node ${node}"
    done
}
