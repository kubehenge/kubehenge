mock_provider "proxmox" {
  mock_resource "proxmox_user_token" {
    defaults = {
      id    = "kubehenge-management-csi@pve!csi"
      value = "kubehenge-management-csi@pve!csi=00000000-0000-0000-0000-000000000000"
    }
  }
  mock_resource "proxmox_download_file" {
    defaults = {
      id = "local:import/talos-v1.14.4-ce4c980550dd-nocloud-amd64.qcow2"
    }
  }
}

mock_provider "talos" {
  mock_resource "talos_image_factory_schematic" {
    defaults = {
      id = "ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515"
    }
  }
  mock_data "talos_image_factory_urls" {
    defaults = {
      urls = {
        disk_image            = "https://factory.talos.dev/image/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515/v1.14.4/nocloud-amd64.qcow2"
        installer             = "factory.talos.dev/nocloud-installer/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515:v1.14.4"
        disk_image_secureboot = ""
        initramfs             = ""
        installer_secureboot  = ""
        iso                   = ""
        iso_secureboot        = ""
        kernel                = ""
        kernel_command_line   = ""
        pxe                   = ""
        uki                   = ""
      }
    }
  }
}

mock_provider "helm" {}

variables {
  proxmox_endpoint       = "https://pve.example.org:8006"
  proxmox_api_token      = "root@pam!test=secret"
  proxmox_ca_certificate = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
  proxmox_nodes          = ["pve1", "pve2"]
  node_addresses         = ["192.0.2.11/24", "192.0.2.12/24", "192.0.2.13/24"]
  api_address            = "192.0.2.10"
  gateway                = "192.0.2.1"
  dns_servers            = ["192.0.2.1"]
  workload_datastores    = ["local-lvm", "ceph"]
  workload_bridges       = ["vmbr0", "vmbr1"]
  vm_datastore           = "fast"
  controller_image       = "ghcr.io/kubehenge/kubehenge@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
}

# These two run first, on an empty state: a setup that stopped after the first node VM.
run "setup_stopped_after_one_node_vm" {
  command = apply

  plan_options {
    target = [proxmox_virtual_environment_vm.node[0]]
  }

  assert {
    condition     = length(proxmox_virtual_environment_vm.node) == 1 && proxmox_virtual_environment_vm.backups.protection
    error_message = "a node VM must come after the protected backups VM, whose disk it may hold"
  }
}

run "lifting_the_backup_volumes_protection_makes_no_node_vm" {
  command = plan

  plan_options {
    target = [proxmox_virtual_environment_vm.backups]
  }

  variables {
    backup_volume = { protection = false }
  }

  assert {
    condition     = !proxmox_virtual_environment_vm.backups.protection && length(proxmox_virtual_environment_vm.node) == 1
    error_message = "the removal's apply targeting the backups VM must lift its protection and make none of the node VMs the setup never made"
  }
}

run "csi_plugin_is_the_workload_clusters_pinned_release" {
  command = apply

  assert {
    condition = (
      helm_release.proxmox_csi.repository == "oci://ghcr.io/sergelogvinov/charts" &&
      helm_release.proxmox_csi.chart == "proxmox-csi-plugin@sha256:${split("@sha256:", yamldecode(file("${path.module}/../charts/kubehenge/values.yaml")).addons.csiPlugin.version)[1]}" &&
      helm_release.proxmox_csi.version == split("@sha256:", yamldecode(file("${path.module}/../charts/kubehenge/values.yaml")).addons.csiPlugin.version)[0] &&
      yamldecode(helm_release.proxmox_csi.values[0]).controller.plugin.image.tag == yamldecode(file("${path.module}/../charts/kubehenge/values.yaml")).addons.csiPlugin.values.controller.plugin.image.tag &&
      yamldecode(helm_release.proxmox_csi.values[0]).node.plugin.image.tag == yamldecode(file("${path.module}/../charts/kubehenge/values.yaml")).addons.csiPlugin.values.node.plugin.image.tag
    )
    error_message = "the management cluster's CSI plugin must be the chart and images the workload clusters' add-on pins, by digest"
  }

  assert {
    condition     = yamldecode(helm_release.proxmox_csi.values[0]).storageClass == [{ name = "kubehenge-management", storage = "fast", reclaimPolicy = "Retain", fstype = "ext4" }]
    error_message = "the storage class must be on the management VMs' storage and keep released volumes"
  }

  assert {
    condition = (
      toset(keys(proxmox_acl.management_csi)) == toset([
        "/nodes kubehenge-audit",
        "/pool/kubehenge-management kubehenge-management-csi",
        "/storage/fast kubehenge-cluster-storage",
      ]) &&
      toset(proxmox_virtual_environment_role.management_csi.privileges) == toset(["VM.Audit", "VM.Config.Disk"]) &&
      !contains(proxmox_virtual_environment_role.cluster_storage.privileges, "Datastore.Allocate") &&
      !proxmox_user_token.management_csi.privileges_separation &&
      proxmox_acl.management_csi_backups.path == "/vms/${proxmox_virtual_environment_vm.backups.vm_id}" &&
      proxmox_acl.management_csi_backups.role_id == "kubehenge-management-csi"
    )
    error_message = "the plugin's token must reach the management pool's VMs and add space on their storage, and delete no volume"
  }

  assert {
    condition = alltrue([for i, n in local.nodes : anytrue([
      for d in split("---\n", data.talos_machine_configuration.node[i].config_patches[1]) :
      try(yamlencode(yamldecode(d).labels), "") == yamlencode({ "topology.kubernetes.io/region" = "kubehenge" })
      if try(yamldecode(d).kind, "") == "KubeNodeConfig"
    ])])
    error_message = "every node must carry the region the plugin registers it by, and no zone, which the appliance alone sets from where the VM runs"
  }
}

run "backup_volume_outlives_the_node_vms" {
  command = apply

  assert {
    condition = (
      proxmox_virtual_environment_vm.backups.protection &&
      !proxmox_virtual_environment_vm.backups.started &&
      !proxmox_virtual_environment_vm.backups.on_boot &&
      !proxmox_virtual_environment_vm.backups.delete_unreferenced_disks_on_destroy &&
      proxmox_virtual_environment_vm.backups.name == "kubehenge-backups" &&
      contains(proxmox_virtual_environment_vm.backups.tags, "backups") &&
      proxmox_virtual_environment_vm.backups.pool_id == null &&
      proxmox_virtual_environment_vm.backups.disk[0].datastore_id == "fast" &&
      proxmox_virtual_environment_vm.backups.disk[0].size == 20
    )
    error_message = "the backup volume must be the protected disk of a stopped, tagged VM of its own on the management VMs' storage"
  }

  assert {
    condition = (
      yamldecode(helm_release.storage.values[0]).backupVolume.claim == "kubehenge-backups" &&
      yamldecode(helm_release.storage.values[0]).backupVolume.namespace == "kubehenge-system" &&
      yamldecode(helm_release.storage.values[0]).backupVolume.handle == "kubehenge/pve1/fast/${proxmox_virtual_environment_vm.backups.disk[0].path_in_datastore}" &&
      yamldecode(helm_release.storage.values[0]).backupVolume.zone == proxmox_virtual_environment_vm.backups.node_name
    )
    error_message = "the backup claim must have its fixed name and bind the backups VM's disk where it is"
  }

  assert {
    condition = (
      yamldecode(base64decode(helm_release.storage.set_sensitive[0].value)).features.controllerVMID == proxmox_virtual_environment_vm.backups.vm_id &&
      yamldecode(base64decode(helm_release.storage.set_sensitive[0].value)).clusters[0].region == "kubehenge"
    )
    error_message = "volumes the plugin makes must belong to the backups VM, so that no node VM's removal deletes them"
  }
}

run "backup_volume_on_shared_storage_attaches_on_any_node" {
  command = apply

  variables {
    backup_volume = { shared = true }
  }

  assert {
    condition = (
      yamldecode(helm_release.storage.values[0]).backupVolume.handle == "kubehenge//fast/${proxmox_virtual_environment_vm.backups.disk[0].path_in_datastore}" &&
      yamldecode(helm_release.storage.values[0]).backupVolume.zone == ""
    )
    error_message = "on shared storage the backup volume must name no zone, so that the store runs on any node"
  }

  assert {
    condition = (
      proxmox_acl.management_zones.path == "/pool/kubehenge-management" &&
      contains([for s in helm_release.storage.set_sensitive : s.name], "backupVolume.zonesToken")
    )
    error_message = "on shared storage too the appliance sets the nodes' zones after an apply, so it must get the token that reads where the VMs run"
  }

  assert {
    condition     = yamldecode(helm_release.storage.values[0]).backupVolume.sharesHost
    error_message = "a Proxmox VE cluster of one node loses shared storage with that node, so the volume shares its host"
  }
}

run "backup_volume_on_shared_storage_of_several_nodes_outlives_a_host" {
  command = apply

  variables {
    backup_volume = { shared = true }
  }

  override_data {
    target = data.proxmox_virtual_environment_nodes.all
    values = { names = ["pve1", "pve2", "pve3"] }
  }

  assert {
    condition     = !yamldecode(helm_release.storage.values[0]).backupVolume.sharesHost
    error_message = "shared storage of several nodes outlives one host, so no warning is due"
  }
}

run "backup_volume_on_local_storage_stays_on_its_node" {
  command = apply

  override_data {
    target = data.proxmox_virtual_environment_nodes.all
    values = { names = ["pve1", "pve2", "pve3"] }
  }

  assert {
    condition     = yamldecode(helm_release.storage.values[0]).backupVolume.zone == "pve1"
    error_message = "on a node's own storage the backup volume must stay on that node"
  }

  assert {
    condition     = yamldecode(helm_release.storage.values[0]).backupVolume.sharesHost
    error_message = "a volume on a node's own storage is lost with that node, so it shares its host"
  }

  assert {
    condition = (
      proxmox_acl.management_zones.path == "/pool/kubehenge-management" &&
      proxmox_acl.management_zones.propagate &&
      toset(proxmox_virtual_environment_role.templates_audit.privileges) == toset(["VM.Audit"]) &&
      proxmox_acl.management_zones.role_id == proxmox_virtual_environment_role.templates_audit.role_id &&
      proxmox_acl.management_zones.user_id == "kubehenge-management-zones@pve" &&
      !proxmox_user_token.management_zones.privileges_separation &&
      one([for s in helm_release.storage.set_sensitive : base64decode(s.value) if s.name == "backupVolume.zonesToken"]) == "kubehenge-management-csi@pve!csi=00000000-0000-0000-0000-000000000000" &&
      yamldecode(helm_release.storage.values[0]).backupVolume.nodes == [for n in local.nodes : n.name]
    )
    error_message = "on a node's own storage the appliance must get a token that only sees the management pool's VMs, and the nodes whose zone it corrects"
  }
}

run "backup_volume_protection_off" {
  command = plan

  variables {
    backup_volume = { protection = false, size_gb = 50 }
  }

  assert {
    condition     = !proxmox_virtual_environment_vm.backups.protection && proxmox_virtual_environment_vm.backups.disk[0].size == 50
    error_message = "protection false must lift the backups VM's protection, so that a destroy removes it with its disk"
  }
}

run "backup_volume_size_is_whole_gb" {
  command = plan

  variables {
    backup_volume = { size_gb = 0.5 }
  }

  expect_failures = [var.backup_volume]
}
