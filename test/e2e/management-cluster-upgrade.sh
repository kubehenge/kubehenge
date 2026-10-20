#!/bin/sh
# End-to-end test of upgrading the management cluster in place: create it on older Talos, Kubernetes
# and provider versions, apply the tested set, and check that the same VMs now run the new versions
# with every provider ready, and that the providers of distributions no cluster uses stay scaled to
# zero across their upgrade. Then destroy and check that nothing is left on Proxmox VE.
#
#   test/e2e/management-cluster-upgrade.sh <directory>
#
# <directory> is as for management-cluster.sh, and must pass version_overrides, provider_images and
# provider_manifests through to management-cluster/ from TF_VAR_version_overrides,
# TF_VAR_provider_images and TF_VAR_provider_manifests.
set -eu

dir=${1:?usage: $0 <directory>}
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

# One step back on each axis: a Talos patch, a Kubernetes minor, a Cluster API minor, a capmox minor.
# The providers held back bring their images and their files' digests, as anyone holding one back
# must.
# shellcheck disable=SC2089,SC2090 # JSON for OpenTofu, not shell words
old='{"talos":"v1.14.3","kubernetes":"v1.36.5","cluster_api":"v1.13.7","capmox":"v0.9.0"}'
# shellcheck disable=SC2089,SC2090 # JSON for OpenTofu, not shell words
old_images='{
  "CoreProvider/cluster-api": "registry.k8s.io/cluster-api/cluster-api-controller:v1.13.7@sha256:330bd56c8bec177bcb91b3a160ff642b45d001f53e10c4fca887b0c04c348d91",
  "BootstrapProvider/kubeadm": "registry.k8s.io/cluster-api/kubeadm-bootstrap-controller:v1.13.7@sha256:1d8ae4cdce642bb934ea5be71066c28709d5fc77969bf81a24c1d324fe787397",
  "ControlPlaneProvider/kubeadm": "registry.k8s.io/cluster-api/kubeadm-control-plane-controller:v1.13.7@sha256:67f55e8a0f6f4c90a2d4a599d632dbc27f6b7d92cd5f1f5e2af7e3106b9382ad",
  "InfrastructureProvider/proxmox": "ghcr.io/ionos-cloud/cluster-api-provider-proxmox:v0.9.0@sha256:373c72cad3ea2bcb10dd7644c37eccb19039c02f586faee994d9c46087a347b5"
}'
# shellcheck disable=SC2089,SC2090 # JSON for OpenTofu, not shell words
old_manifests='{
  "kubernetes-sigs/cluster-api/releases/download/v1.13.7/metadata.yaml": "20ffd682bfc50ab5193aa14ba55222ba390f9204ccf6a0dd2d52c4a3c4fb004f",
  "kubernetes-sigs/cluster-api/releases/download/v1.13.7/core-components.yaml": "268007c14fd9c6e5e790dc5274ab2befd2bab02761261d2860dc47756a7a20f2",
  "kubernetes-sigs/cluster-api/releases/download/v1.13.7/bootstrap-components.yaml": "7ee336e138208949f9b772895ec35adb9f5694012d2a4d82c04f0106f0eb1f94",
  "kubernetes-sigs/cluster-api/releases/download/v1.13.7/control-plane-components.yaml": "1988d462cf5d6abb77ae105eaaf230b5a20e01587067bbc5b602b2f1026f21e1",
  "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.0/metadata.yaml": "df2916a79503ac053f6a9866a8de025e780e24d6d472f00f59f84898b5a1c572",
  "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.0/infrastructure-components.yaml": "43b229d747ffb0adee67d4c3a4be20859779332fcaaf8d195205e1e41e53028f"
}'
# The tested set's versions; the same names also map to the providers' repositories there.
new_talos=$(sed -n 's/^ *talos *= *"\(v[0-9][^"]*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")
new_kubernetes=$(sed -n 's/^ *kubernetes *= *"\(v[0-9][^"]*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")
new_cluster_api=$(sed -n 's/^ *cluster_api *= *"\(v[0-9][^"]*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")
new_capmox=$(sed -n 's/^ *capmox *= *"\(v[0-9][^"]*\)"/\1/p' "$(dirname "$0")/../../management-cluster/versions.tf")

step "apply the older versions"
export TF_VAR_version_overrides="${old}" TF_VAR_provider_images="${old_images}" TF_VAR_provider_manifests="${old_manifests}"
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
export TF_VAR_version_overrides='{}' TF_VAR_provider_images='{}' TF_VAR_provider_manifests='{}'
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
