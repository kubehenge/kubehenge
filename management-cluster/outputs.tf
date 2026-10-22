output "kubeconfig" {
  description = "Kubeconfig of the management cluster."
  value       = talos_cluster_kubeconfig.this.kubeconfig_raw
  sensitive   = true
}

output "talosconfig" {
  description = "talosctl configuration for the management cluster's nodes."
  value       = data.talos_client_configuration.this.talos_config
  sensitive   = true
}

output "proxmox_objects" {
  description = "What this configuration created on Proxmox VE, so a destroy can be checked."
  value = {
    endpoint      = var.proxmox_endpoint
    nodes         = var.proxmox_nodes
    pools         = [proxmox_virtual_environment_pool.management.pool_id, proxmox_virtual_environment_pool.workload.pool_id, proxmox_virtual_environment_pool.templates.pool_id]
    workload_pool = proxmox_virtual_environment_pool.workload.pool_id
    template_pool = proxmox_virtual_environment_pool.templates.pool_id
    # Where the weekly build puts the templates' disks.
    template_storage = local.template_builder.storage
    roles = concat([
      proxmox_virtual_environment_role.audit.role_id,
      proxmox_virtual_environment_role.allocate_space.role_id,
      proxmox_virtual_environment_role.cluster_vms.role_id,
      proxmox_virtual_environment_role.cluster_storage.role_id,
      proxmox_virtual_environment_role.provisioner_pools.role_id,
      proxmox_virtual_environment_role.provisioner_users.role_id,
      proxmox_virtual_environment_role.provisioner_realm.role_id,
      proxmox_virtual_environment_role.templates_audit.role_id,
      ], proxmox_virtual_environment_role.ha[*].role_id,
      proxmox_virtual_environment_role.cluster_datastore[*].role_id,
      proxmox_virtual_environment_role.template_builder_vms[*].role_id,
      proxmox_virtual_environment_role.template_builder_files[*].role_id,
      proxmox_virtual_environment_role.template_builder_network[*].role_id,
    )
    users = concat(
      [proxmox_virtual_environment_user.cluster_api.user_id, proxmox_virtual_environment_user.provisioner.user_id],
      proxmox_virtual_environment_user.ha[*].user_id,
      proxmox_virtual_environment_user.template_builder[*].user_id,
    )
    groups = [proxmox_virtual_environment_group.clusters.group_id]
    vm_ids = proxmox_virtual_environment_vm.node[*].vm_id
    # The Talos image downloaded to each node, by node.
    images = { for node, d in proxmox_download_file.talos : node => d.id }
  }
}

output "api_endpoint" {
  description = "Kubernetes API of the management cluster."
  value       = local.api_endpoint
}
