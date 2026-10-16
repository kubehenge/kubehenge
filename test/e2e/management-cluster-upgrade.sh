#!/bin/sh
# End-to-end test of upgrading the management cluster in place: create it on older Talos, Kubernetes
# and provider versions, apply the tested set, and check that the same VMs now run the new versions
# with every provider ready, and that the providers of distributions no cluster uses stay scaled to
# zero across their upgrade. Then destroy and check that nothing is left on Proxmox VE.
#
#   test/e2e/management-cluster-upgrade.sh <directory>
#
# <directory> is as for management-cluster.sh, and must pass version_overrides through to
# management-cluster/ from TF_VAR_version_overrides.
set -eu

dir=${1:?usage: $0 <directory>}
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

# One step back on each axis: a Talos patch, a Kubernetes minor, a Cluster API minor, a capmox minor.
# shellcheck disable=SC2089,SC2090 # JSON for OpenTofu, not shell words
old='{"talos":"v1.14.3","kubernetes":"v1.36.5","cluster_api":"v1.13.7","capmox":"v0.9.0"}'
new_talos=$(sed -n 's/^ *talos *= *"\(.*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")
new_kubernetes=$(sed -n 's/^ *kubernetes *= *"\(.*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")
new_cluster_api=$(sed -n 's/^ *cluster_api *= *"\(.*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")
new_capmox=$(sed -n 's/^ *capmox *= *"\(.*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")

step "apply the older versions"
export TF_VAR_version_overrides="${old}"
apply_management_cluster
check_nodes
check_providers
vms_before=$(jq -c '.vm_ids | sort' "${work}/objects.json")
cluster_api=$(installed_version cluster-api)
[ "${cluster_api}" = v1.13.7 ] || fail "Cluster API is ${cluster_api}, not v1.13.7"
capmox=$(installed_version proxmox)
[ "${capmox}" = v0.9.0 ] || fail "capmox is ${capmox}, not v0.9.0"
# No cluster uses a distribution, so Kubehenge's controller keeps its providers scaled to zero; the
# kubeadm ones move to the new Cluster API release below and must stay at zero through it.
classes="kubehenge-kubeadm kubehenge-rke2 kubehenge-k3s"
for class in ${classes}; do
    until_true 300 "${class}'s providers idle" providers_idle "${class}"
done

step "upgrade to the tested set"
# shellcheck disable=SC2090 # JSON for OpenTofu, not shell words
export TF_VAR_version_overrides='{}'
apply_management_cluster
check_nodes
check_providers
check_topology_and_tls
for class in ${classes}; do
    until_true 300 "${class}'s providers idle after their upgrade" providers_idle "${class}"
done

step "same VMs, new versions"
vms_after=$(jq -c '.vm_ids | sort' "${work}/objects.json")
[ "${vms_after}" = "${vms_before}" ] || fail 'the upgrade replaced VMs'
kubectl get nodes -o json | jq -e --arg t "Talos (${new_talos})" '[.items[].status.nodeInfo.osImage] | all(. == $t)' > /dev/null ||
    fail "nodes do not all run Talos ${new_talos}"
kubectl get nodes -o json | jq -e --arg k "${new_kubernetes}" '[.items[].status.nodeInfo.kubeletVersion] | all(. == $k)' > /dev/null ||
    fail "kubelets are not all at ${new_kubernetes}"
kubectl version -o json | jq -e --arg k "${new_kubernetes}" '.serverVersion.gitVersion == $k' > /dev/null ||
    fail "the API server is not at ${new_kubernetes}"
cluster_api=$(installed_version cluster-api)
[ "${cluster_api}" = "${new_cluster_api}" ] || fail "Cluster API is ${cluster_api}, not ${new_cluster_api}"
capmox=$(installed_version proxmox)
[ "${capmox}" = "${new_capmox}" ] || fail "capmox is ${capmox}, not ${new_capmox}"
check_capmox_privileges

step destroy
destroy_and_check_nothing_left

printf '\nPASS\n'
