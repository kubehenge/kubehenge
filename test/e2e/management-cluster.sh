#!/bin/sh
# End-to-end test of the management cluster on a Proxmox VE host: apply, check that Talos, Kubernetes
# and every Cluster API provider come up, destroy, and check that nothing is left on Proxmox VE.
#
#   test/e2e/management-cluster.sh <directory> [command...]
#
# <directory> is a Terraform/OpenTofu root configuration that applies management-cluster/ (directly,
# with a terraform.tfvars, or as a module) and passes on its kubeconfig and proxmox_objects outputs.
# TF_VAR_proxmox_api_token must be set; it is also used to inspect Proxmox VE after the destroy.
# TOFU selects the binary (default tofu). A command given after the directory runs once the checks
# pass and before the destroy, with KUBECONFIG set to the management cluster's; the test fails if it
# does, and what it creates has to be gone again for the check after the destroy to pass.
set -eu

dir=${1:?usage: $0 <directory> [command...]}
shift
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

step apply
apply_management_cluster

step nodes
check_nodes

step providers
check_providers

step "cluster topology and TLS verification"
check_topology_and_tls

step "Kubehenge's controller and ClusterClasses"
check_kubehenge

step "capmox signs in to Proxmox VE with least privilege"
check_capmox_privileges

if [ $# -gt 0 ]; then
    step "while the management cluster is up: $*"
    "$@"
fi

step destroy
destroy_and_check_nothing_left

printf '\nPASS\n'
