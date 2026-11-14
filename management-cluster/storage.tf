# Persistent storage in the management cluster: the Proxmox CSI plugin, from the same pinned chart and
# images the workload clusters' add-on uses, and the volume the management backups are kept on.

locals {
  csi_plugin = yamldecode(file("${path.module}/../charts/kubehenge/values.yaml")).addons.csiPlugin
  # The plugin matches the nodes' topology.kubernetes.io/region label against this name.
  csi_region = "kubehenge"
  # The storage class every volume of the management cluster is made on.
  storage_class = "kubehenge-management"
  storage_secret = {
    name      = "kubehenge-proxmox-csi"
    namespace = "kube-system"
  }
  backup_volume = {
    claim     = "kubehenge-backups"
    namespace = local.kubehenge_namespace
    node      = local.nodes[0].proxmox_node
    # Shared storage (Ceph RBD, NFS, shared LVM) attaches the disk on any node. Its handle names no
    # zone, the plugin's own form for shared volumes, so the store runs wherever a node is up.
    shared = coalesce(var.backup_volume.shared, try(data.proxmox_datastores.backup_volume.datastores[0].shared, false), false)
  }
  backup_volume_zone = local.backup_volume.shared ? "" : local.backup_volume.node
  # Losing one Proxmox VE host loses the volume on that host's own storage, and every disk where the
  # cluster has one node. Only a bucket survives that, which the interface then suggests.
  backup_volume_shares_host = !local.backup_volume.shared || length(data.proxmox_virtual_environment_nodes.all.names) < 2
}

data "proxmox_virtual_environment_nodes" "all" {}

data "proxmox_datastores" "backup_volume" {
  node_name = local.nodes[0].proxmox_node
  filters = {
    id = var.vm_datastore
  }
}

# The plugin finds a node's VM by its SMBIOS UUID (VM.Audit), attaches and detaches disks
# (VM.Config.Disk) and makes volumes on the management VMs' storage (Datastore.AllocateSpace). Not
# Datastore.Allocate: every claim is Retain, so the plugin never deletes a volume, and that
# privilege would reach every volume on the storage.
resource "proxmox_virtual_environment_role" "management_csi" {
  role_id    = "${var.cluster_name}-management-csi"
  privileges = ["VM.Audit", "VM.Config.Disk"]
}

resource "proxmox_virtual_environment_user" "management_csi" {
  user_id = "${var.cluster_name}-management-csi@pve"
  comment = "The CSI plugin in Kubehenge management cluster ${var.cluster_name}; signs in with its API token only"
}

resource "proxmox_user_token" "management_csi" {
  user_id               = proxmox_virtual_environment_user.management_csi.user_id
  token_name            = "csi"
  comment               = "The management cluster's CSI plugin"
  privileges_separation = false
}

locals {
  management_csi_acls = [
    { path = "/nodes", role = proxmox_virtual_environment_role.audit.role_id, propagate = true },
    { path = "/pool/${proxmox_virtual_environment_pool.management.pool_id}", role = proxmox_virtual_environment_role.management_csi.role_id, propagate = true },
    { path = "/storage/${var.vm_datastore}", role = proxmox_virtual_environment_role.cluster_storage.role_id, propagate = false },
  ]
}

resource "proxmox_acl" "management_csi" {
  for_each = { for a in local.management_csi_acls : "${a.path} ${a.role}" => a }

  user_id   = proxmox_virtual_environment_user.management_csi.user_id
  path      = each.value.path
  role_id   = each.value.role
  propagate = each.value.propagate
}

# The plugin attaches the backups VM's disk to the node VMs and makes its volumes under that VM.
resource "proxmox_acl" "management_csi_backups" {
  user_id   = proxmox_virtual_environment_user.management_csi.user_id
  path      = "/vms/${proxmox_virtual_environment_vm.backups.vm_id}"
  role_id   = proxmox_virtual_environment_role.management_csi.role_id
  propagate = false
}

# On a node's own storage the plugin attaches the backup volume only to a node labelled with that
# node's zone. HA or an administrator may move a node VM, and the label still names the old node.
# Before it wakes the store, the appliance reads where each node VM runs and corrects the label.
# This token is all it holds for that: VM.Audit (the templates' audit role) on the management pool.
resource "proxmox_virtual_environment_user" "management_zones" {
  count = local.backup_volume.shared ? 0 : 1

  user_id = "${var.cluster_name}-management-zones@pve"
  comment = "Reads where Kubehenge management cluster ${var.cluster_name}'s VMs run; signs in with its API token only"
}

resource "proxmox_user_token" "management_zones" {
  count = local.backup_volume.shared ? 0 : 1

  user_id               = proxmox_virtual_environment_user.management_zones[0].user_id
  token_name            = "zones"
  comment               = "Where the management cluster's VMs run"
  privileges_separation = false
}

resource "proxmox_acl" "management_zones" {
  count = local.backup_volume.shared ? 0 : 1

  user_id   = proxmox_virtual_environment_user.management_zones[0].user_id
  path      = "/pool/${proxmox_virtual_environment_pool.management.pool_id}"
  role_id   = proxmox_virtual_environment_role.templates_audit.role_id
  propagate = true
}

# The backup volume is the one disk of a VM that never starts. Proxmox VE deletes a disk with a VM
# only when the VM owns it, so losing or destroying every node VM, which the plugin attaches it to,
# leaves it. Protection refuses deleting this VM or its disk until it is lifted. It sits in no pool,
# so that it can stay when the management pool goes.
resource "proxmox_virtual_environment_vm" "backups" {
  name        = "${var.cluster_name}-backups"
  description = "The backup volume of Kubehenge management cluster ${var.cluster_name}: etcd snapshots and exports. Protected, kept when the management cluster is removed."
  tags        = ["kubehenge", "management", "backups"]
  node_name   = local.backup_volume.node

  # Destroyed after the node VMs, which may still have its disk attached.
  depends_on = [proxmox_virtual_environment_vm.node]

  started    = false
  on_boot    = false
  protection = var.backup_volume.protection
  # Only the VM's own disk goes with it. A volume the plugin made elsewhere stays.
  delete_unreferenced_disks_on_destroy = false

  scsi_hardware = "virtio-scsi-single"

  disk {
    datastore_id = var.vm_datastore
    interface    = "scsi0"
    size         = var.backup_volume.size_gb
    file_format  = "raw"
    discard      = "on"
    ssd          = true
  }

  operating_system {
    type = "l26"
  }
}

locals {
  storage_values = {
    chartDigest = local.chart_digests.storage
    secret      = local.storage_secret.name
    image       = local.controller_image
    caCert      = var.proxmox_ca_certificate
    storageClass = {
      name    = local.storage_class
      storage = var.vm_datastore
    }
    backupVolume = {
      claim     = local.backup_volume.claim
      namespace = local.backup_volume.namespace
      sizeGB    = var.backup_volume.size_gb
      handle    = "${local.csi_region}/${local.backup_volume_zone}/${var.vm_datastore}/${proxmox_virtual_environment_vm.backups.disk[0].path_in_datastore}"
      zone      = local.backup_volume_zone
      region    = local.csi_region
      # The nodes whose zone label the appliance may correct.
      nodes      = local.nodes[*].name
      sharesHost = local.backup_volume_shares_host
    }
  }
}

# The plugin's configuration and the backup volume's PersistentVolume and claim. The volume and its
# claim are kept when the release goes, so that an uninstall of the release never touches them.
resource "helm_release" "storage" {
  name      = "kubehenge-storage"
  namespace = local.storage_secret.namespace
  chart     = "${path.module}/charts/storage"
  # The backup store becomes ready only once the plugin, installed after this release, attaches
  # the volume. The plugin's release waits for its own pods instead.
  wait = false

  values = [yamlencode(local.storage_values)]

  # Base64, which Helm's --set parsing leaves as it is.
  set_sensitive = concat([{
    name = "encodedConfig"
    value = base64encode(yamlencode({
      features = { provider = "capmox", controllerVMID = proxmox_virtual_environment_vm.backups.vm_id }
      clusters = [{
        url          = "${trimsuffix(var.proxmox_endpoint, "/")}/api2/json"
        insecure     = var.proxmox_insecure
        token_id     = proxmox_user_token.management_csi.id
        token_secret = reverse(split("=", proxmox_user_token.management_csi.value))[0]
        region       = local.csi_region
      }]
    }))
    type = "string"
    }], [for t in proxmox_user_token.management_zones : {
    name  = "backupVolume.zonesToken"
    value = base64encode("${t.id}=${reverse(split("=", t.value))[0]}")
    type  = "string"
  }])

  depends_on = [helm_release.cluster_api_providers, proxmox_acl.management_csi, proxmox_acl.management_csi_backups, proxmox_acl.management_zones]
}

resource "helm_release" "proxmox_csi" {
  name       = "proxmox-csi"
  namespace  = local.storage_secret.namespace
  repository = local.csi_plugin.repository
  chart      = "${local.csi_plugin.chart}@sha256:${split("@sha256:", local.csi_plugin.version)[1]}"
  version    = split("@sha256:", local.csi_plugin.version)[0]
  wait       = true
  timeout    = 900

  values = [yamlencode(merge(local.csi_plugin.values, {
    existingConfigSecret    = local.storage_secret.name
    existingConfigSecretKey = "config.yaml"
    # The chart has no CA setting. The plugin talks to Proxmox VE and the in-cluster API, which
    # uses its own CA, so the Proxmox VE CA takes the image's trust store's place.
    extraVolumes      = [{ name = "proxmox-ca", secret = { secretName = local.storage_secret.name, items = [{ key = "ca.crt", path = "ca.crt" }] } }]
    extraVolumeMounts = [{ name = "proxmox-ca", mountPath = "/etc/ssl/certs/ca-certificates.crt", subPath = "ca.crt", readOnly = true }]
    storageClass = [{
      name          = local.storage_class
      storage       = var.vm_datastore
      reclaimPolicy = "Retain"
      fstype        = "ext4"
    }]
  }))]

  depends_on = [helm_release.storage]
}
