resource "proxmox_virtual_environment_pool" "management" {
  pool_id = var.management_pool
  comment = "Kubehenge management cluster ${var.cluster_name}"
}

resource "proxmox_virtual_environment_pool" "workload" {
  pool_id = var.workload_pool
  comment = "Clusters created by Kubehenge management cluster ${var.cluster_name}"
}

resource "proxmox_virtual_environment_pool" "templates" {
  pool_id = var.template_pool
  comment = "VM templates for clusters created by Kubehenge management cluster ${var.cluster_name}"
}

# Cluster API's Proxmox VE provider gets its own token, limited to the pools, storages and bridges
# above; it can neither see nor touch the management cluster's own VMs.
resource "proxmox_virtual_environment_role" "audit" {
  role_id    = "${var.cluster_name}-audit"
  privileges = ["Sys.Audit"]
}

# Seeing the templates, so the controller knows which Kubernetes versions a kubeadm cluster can be
# upgraded to.
resource "proxmox_virtual_environment_role" "templates_audit" {
  role_id    = "${var.cluster_name}-templates-audit"
  privileges = ["VM.Audit"]
}

resource "proxmox_virtual_environment_role" "allocate_space" {
  role_id    = "${var.cluster_name}-allocate-space"
  privileges = ["Datastore.AllocateSpace"]
}

resource "proxmox_virtual_environment_user" "cluster_api" {
  user_id = "${var.cluster_name}-cluster-api@pve"
  comment = "Cluster API in Kubehenge management cluster ${var.cluster_name}; signs in with its API token only"
}

# The user has no password and exists only for this token, so the token carries the user's
# privileges; a separated token would hold only the intersection of its own ACLs and the user's.
resource "proxmox_user_token" "cluster_api" {
  user_id               = proxmox_virtual_environment_user.cluster_api.user_id
  token_name            = "capmox"
  comment               = "Cluster API Proxmox VE provider"
  privileges_separation = false
}

locals {
  cluster_api_acls = concat(
    [
      { path = "/", role = proxmox_virtual_environment_role.audit.role_id, propagate = false },
      { path = "/nodes", role = proxmox_virtual_environment_role.audit.role_id, propagate = true },
      # Propagated: each workload cluster's VMs sit in a pool of their own nested under this one, and
      # a pool's ACL reaches nested pools only when propagated.
      { path = "/pool/${proxmox_virtual_environment_pool.workload.pool_id}", role = "PVEVMAdmin", propagate = true },
      { path = "/pool/${proxmox_virtual_environment_pool.templates.pool_id}", role = "PVETemplateUser", propagate = false },
      { path = "/storage/${var.bootstrap_datastore}", role = "PVEDatastoreAdmin", propagate = false },
    ],
    [for s in var.workload_datastores : { path = "/storage/${s}", role = proxmox_virtual_environment_role.allocate_space.role_id, propagate = false }],
    [for b in var.workload_bridges : { path = "/sdn/zones/localnetwork/${b}", role = "PVESDNUser", propagate = true }],
  )
}

resource "proxmox_acl" "cluster_api" {
  for_each = { for a in local.cluster_api_acls : "${a.path} ${a.role}" => a }

  user_id   = proxmox_virtual_environment_user.cluster_api.user_id
  path      = each.value.path
  role_id   = each.value.role
  propagate = each.value.propagate
}

# Each workload cluster gets a user of its own, created by Kubehenge's controller in this group, with
# rights on its own pool only; the storage and node rights all clusters need come from the group.
resource "proxmox_virtual_environment_role" "cluster_vms" {
  role_id = "${var.cluster_name}-cluster-vms"
  # VM.Audit for the cloud controller manager; VM.Config.Disk for the CSI plugin, on the cluster's
  # own VMs and the VMs that own its volumes; VM.Clone for the proxy beside the plugin, which copies
  # a volume for a snapshot, a restore or a clone by importing it into a disk of one of the
  # cluster's holder VMs, and Proxmox VE asks VM.Clone on the VM the copied volume belongs to. Not
  # VM.Allocate, without which no VM can be cloned.
  privileges = ["VM.Audit", "VM.Clone", "VM.Config.Disk"]
}

resource "proxmox_virtual_environment_role" "cluster_storage" {
  role_id = "${var.cluster_name}-cluster-storage"
  # Not Datastore.Allocate: that would reach every volume on the storage, other clusters' and the
  # management cluster's disks included, and the storage's own definition.
  privileges = ["Datastore.AllocateSpace", "Datastore.Audit"]
}

# What a cluster that opts in to the CSI plugin's newest release holds on its own storage: the
# plugin reads a storage's configuration and deletes volumes through the storage, both of which
# Proxmox VE allows only with Datastore.Allocate. An ACL of a user's own on a path takes the place of
# its group's there, so the role carries the group's privileges too.
resource "proxmox_virtual_environment_role" "cluster_datastore" {
  count = var.cluster_datastore_allocate ? 1 : 0

  role_id    = "${var.cluster_name}-cluster-datastore"
  privileges = ["Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.Audit"]
}

resource "proxmox_virtual_environment_group" "clusters" {
  group_id = "${var.cluster_name}-clusters"
  comment  = "Clusters created by Kubehenge management cluster ${var.cluster_name}"
}

locals {
  clusters_acls = concat(
    [for s in var.workload_datastores : { path = "/storage/${s}", role = proxmox_virtual_environment_role.cluster_storage.role_id, propagate = false }],
    [{ path = "/nodes", role = proxmox_virtual_environment_role.audit.role_id, propagate = true }],
  )
}

resource "proxmox_acl" "clusters" {
  for_each = { for a in local.clusters_acls : "${a.path} ${a.role}" => a }

  group_id  = proxmox_virtual_environment_group.clusters.group_id
  path      = each.value.path
  role_id   = each.value.role
  propagate = each.value.propagate
}

# The cloud controller manager lists HA groups and rules for its HA group labels, which Proxmox VE
# allows only with Sys.Audit on /. The controller adds the user of each cluster with the labels to
# this group: granting anything on / itself would take Permissions.Modify on /, with which the
# controller could grant itself every privilege.
resource "proxmox_virtual_environment_group" "clusters_audit" {
  count = var.cluster_sys_audit ? 1 : 0

  group_id = "${var.cluster_name}-clusters-audit"
  comment  = "Clusters created by Kubehenge management cluster ${var.cluster_name} that label their nodes with Proxmox VE HA groups"
}

resource "proxmox_acl" "clusters_audit" {
  count = var.cluster_sys_audit ? 1 : 0

  group_id  = proxmox_virtual_environment_group.clusters_audit[0].group_id
  path      = "/"
  role_id   = proxmox_virtual_environment_role.audit.role_id
  propagate = false
}

# Kubehenge's controller creates each cluster's pool, user, token, ACL and volume owner VM, and
# nothing outside the workload pool and the clusters' group. VM.Migrate and VM.Config.Disk move a
# volume owner VM to the node of a volume on node-local storage and delete the volume through it;
# VM.Allocate there already lets it destroy the VMs outright.
resource "proxmox_virtual_environment_role" "provisioner_pools" {
  role_id    = "${var.cluster_name}-provisioner-pools"
  privileges = ["Pool.Allocate", "Pool.Audit", "Permissions.Modify", "VM.Allocate", "VM.Audit", "VM.Config.Disk", "VM.Config.Options", "VM.Migrate"]
}

resource "proxmox_virtual_environment_role" "provisioner_users" {
  role_id    = "${var.cluster_name}-provisioner-users"
  privileges = ["User.Modify"]
}

resource "proxmox_virtual_environment_role" "provisioner_realm" {
  role_id    = "${var.cluster_name}-provisioner-realm"
  privileges = ["Realm.AllocateUser"]
}

resource "proxmox_virtual_environment_user" "provisioner" {
  user_id = "${var.cluster_name}-provisioner@pve"
  comment = "Kubehenge's controller in management cluster ${var.cluster_name}; signs in with its API token only"
}

resource "proxmox_user_token" "provisioner" {
  user_id               = proxmox_virtual_environment_user.provisioner.user_id
  token_name            = "controller"
  comment               = "Kubehenge's controller"
  privileges_separation = false
}

# Proxmox VE lets only Sys.Console on / create HA resources and rules. That is also a root shell on
# every node, so it goes to a user and token of their own, which the controller uses for HA alone.
resource "proxmox_virtual_environment_role" "ha" {
  count = var.proxmox_ha ? 1 : 0

  role_id    = "${var.cluster_name}-ha"
  privileges = ["Sys.Audit", "Sys.Console"]
}

resource "proxmox_virtual_environment_user" "ha" {
  count = var.proxmox_ha ? 1 : 0

  user_id = "${var.cluster_name}-ha@pve"
  comment = "Proxmox VE HA for the clusters of Kubehenge management cluster ${var.cluster_name}; signs in with its API token only"
}

resource "proxmox_user_token" "ha" {
  count = var.proxmox_ha ? 1 : 0

  user_id               = proxmox_virtual_environment_user.ha[0].user_id
  token_name            = "ha"
  comment               = "Kubehenge's controller, for Proxmox VE HA"
  privileges_separation = false
}

resource "proxmox_acl" "ha" {
  count = var.proxmox_ha ? 1 : 0

  user_id   = proxmox_virtual_environment_user.ha[0].user_id
  path      = "/"
  role_id   = proxmox_virtual_environment_role.ha[0].role_id
  propagate = false
}

# Restoring a cluster's etcd runs commands as root in its control-plane VMs through the guest
# agent, which VM.GuestAgent.Unrestricted allows in every VM of the workload pool: a user and token
# of their own, which the controller uses for restores alone.
resource "proxmox_virtual_environment_role" "etcd_restore" {
  count = var.etcd_restore ? 1 : 0

  role_id    = "${var.cluster_name}-etcd-restore"
  privileges = ["VM.Audit", "VM.GuestAgent.Unrestricted"]
}

resource "proxmox_virtual_environment_user" "etcd_restore" {
  count = var.etcd_restore ? 1 : 0

  user_id = "${var.cluster_name}-etcd-restore@pve"
  comment = "etcd restores for the clusters of Kubehenge management cluster ${var.cluster_name}; signs in with its API token only"
}

resource "proxmox_user_token" "etcd_restore" {
  count = var.etcd_restore ? 1 : 0

  user_id               = proxmox_virtual_environment_user.etcd_restore[0].user_id
  token_name            = "etcd-restore"
  comment               = "Kubehenge's controller, for etcd restores"
  privileges_separation = false
}

resource "proxmox_acl" "etcd_restore" {
  count = var.etcd_restore ? 1 : 0

  user_id   = proxmox_virtual_environment_user.etcd_restore[0].user_id
  path      = "/pool/${proxmox_virtual_environment_pool.workload.pool_id}"
  role_id   = proxmox_virtual_environment_role.etcd_restore[0].role_id
  propagate = true
}

locals {
  # The storages the management cluster's own VMs, its disk image and Cluster API's cloud-init
  # drives of every cluster are on, and those the template builds download and upload to with
  # Datastore.Allocate of their own, which no cluster may hold Datastore.Allocate on.
  management_storages = distinct(concat(
    [var.vm_datastore, var.image_datastore, var.bootstrap_datastore],
    local.template_builder.enabled ? [local.template_builder.import, local.template_builder.iso] : [],
  ))
  datastore_allocate_storages = var.cluster_datastore_allocate ? [for s in var.workload_datastores : s if !contains(local.management_storages, s)] : []

  provisioner_acls = concat(
    [
      { path = "/", role = proxmox_virtual_environment_role.audit.role_id, propagate = false },
      { path = "/nodes", role = proxmox_virtual_environment_role.audit.role_id, propagate = true },
      { path = "/pool/${proxmox_virtual_environment_pool.workload.pool_id}", role = proxmox_virtual_environment_role.provisioner_pools.role_id, propagate = true },
      { path = "/access/groups/${proxmox_virtual_environment_group.clusters.group_id}", role = proxmox_virtual_environment_role.provisioner_users.role_id, propagate = false },
      { path = "/access/realm/pve", role = proxmox_virtual_environment_role.provisioner_realm.role_id, propagate = false },
      { path = "/pool/${proxmox_virtual_environment_pool.templates.pool_id}", role = proxmox_virtual_environment_role.templates_audit.role_id, propagate = true },
    ],
    # Finding and deleting a removed cluster's volumes on storages local to nodes other than its
    # volume owner VM's, which destroying that VM does not reach.
    [for s in var.workload_datastores : { path = "/storage/${s}", role = proxmox_virtual_environment_role.cluster_storage.role_id, propagate = false }],
    # Granting a cluster's user the role on its storage. Without Permissions.Modify, Proxmox VE lets a
    # user grant on a storage only privileges it holds there, with Datastore.Allocate.
    [for s in local.datastore_allocate_storages : { path = "/storage/${s}", role = proxmox_virtual_environment_role.cluster_datastore[0].role_id, propagate = false }],
    # Adding a cluster's user to the group holding Sys.Audit on /, and taking it out again.
    [for g in proxmox_virtual_environment_group.clusters_audit : { path = "/access/groups/${g.group_id}", role = proxmox_virtual_environment_role.provisioner_users.role_id, propagate = false }],
  )
}

resource "proxmox_acl" "provisioner" {
  for_each = { for a in local.provisioner_acls : "${a.path} ${a.role}" => a }

  user_id   = proxmox_virtual_environment_user.provisioner.user_id
  path      = each.value.path
  role_id   = each.value.role
  propagate = each.value.propagate
}

# The template refresh builds the node OS templates again every week with a user and token of their
# own, so that nothing else in the management cluster can make or delete templates. Deleting a
# downloaded image or a cloud-init ISO takes Datastore.Allocate on its storage, which reaches every
# file there; Sys.AccessNetwork on the node lets Proxmox VE download the image, and nothing more.
locals {
  template_builder = {
    enabled = var.template_refresh.enabled
    node    = coalesce(var.template_refresh.node, var.proxmox_nodes[0])
    storage = coalesce(var.template_refresh.storage, var.workload_datastores[0])
    import  = coalesce(var.template_refresh.import_datastore, var.image_datastore)
    iso     = coalesce(var.template_refresh.iso_datastore, var.bootstrap_datastore)
    bridge  = coalesce(var.template_refresh.bridge, var.workload_bridges[0])
  }
}

resource "proxmox_virtual_environment_role" "template_builder_vms" {
  count = local.template_builder.enabled ? 1 : 0

  role_id = "${var.cluster_name}-template-builder-vms"
  # Creating, preparing (through the guest agent), converting and deleting the template VMs.
  # Pool.Audit because the cluster's resource view leaves out the pool of every VM in a pool the
  # caller may not audit, and the builder tells its own builds and templates by that pool.
  privileges = [
    "Pool.Audit", "VM.Allocate", "VM.Audit", "VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Disk", "VM.Config.HWType",
    "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "VM.GuestAgent.Unrestricted", "VM.PowerMgmt",
  ]
}

resource "proxmox_virtual_environment_role" "template_builder_files" {
  count = local.template_builder.enabled ? 1 : 0

  role_id    = "${var.cluster_name}-template-builder-files"
  privileges = ["Datastore.Allocate", "Datastore.AllocateTemplate", "Datastore.Audit"]
}

resource "proxmox_virtual_environment_role" "template_builder_network" {
  count = local.template_builder.enabled ? 1 : 0

  role_id    = "${var.cluster_name}-template-builder-network"
  privileges = ["Sys.AccessNetwork", "Sys.Audit"]
}

resource "proxmox_virtual_environment_user" "template_builder" {
  count = local.template_builder.enabled ? 1 : 0

  user_id = "${var.cluster_name}-template-builder@pve"
  comment = "VM template builds of Kubehenge management cluster ${var.cluster_name}; signs in with its API token only"
}

resource "proxmox_user_token" "template_builder" {
  count = local.template_builder.enabled ? 1 : 0

  user_id               = proxmox_virtual_environment_user.template_builder[0].user_id
  token_name            = "refresh"
  comment               = "Kubehenge's template refresh"
  privileges_separation = false
}

locals {
  template_builder_acls = local.template_builder.enabled ? concat(
    [
      { path = "/", role = proxmox_virtual_environment_role.audit.role_id, propagate = false },
      { path = "/nodes", role = proxmox_virtual_environment_role.audit.role_id, propagate = true },
      { path = "/nodes/${local.template_builder.node}", role = proxmox_virtual_environment_role.template_builder_network[0].role_id, propagate = false },
      { path = "/pool/${proxmox_virtual_environment_pool.templates.pool_id}", role = proxmox_virtual_environment_role.template_builder_vms[0].role_id, propagate = true },
      { path = "/sdn/zones/localnetwork/${local.template_builder.bridge}", role = "PVESDNUser", propagate = true },
    ],
    [for s in distinct([local.template_builder.import, local.template_builder.iso]) : { path = "/storage/${s}", role = proxmox_virtual_environment_role.template_builder_files[0].role_id, propagate = false }],
    # The template disks' storage, also when it is one of those two: Datastore.Allocate there does
    # not hold Datastore.AllocateSpace, which creating a disk needs.
    [{ path = "/storage/${local.template_builder.storage}", role = proxmox_virtual_environment_role.cluster_storage.role_id, propagate = false }],
  ) : []
}

resource "proxmox_acl" "template_builder" {
  for_each = { for a in local.template_builder_acls : "${a.path} ${a.role}" => a }

  user_id   = proxmox_virtual_environment_user.template_builder[0].user_id
  path      = each.value.path
  role_id   = each.value.role
  propagate = each.value.propagate
}

resource "proxmox_download_file" "talos" {
  for_each = toset(var.proxmox_nodes)

  node_name    = each.value
  datastore_id = var.image_datastore
  content_type = "import"
  file_name    = "talos-${local.versions.talos}-${substr(talos_image_factory_schematic.this.id, 0, 12)}-nocloud-amd64.qcow2"
  url          = data.talos_image_factory_urls.this.urls.disk_image
  overwrite    = false

  # The provider first asks Proxmox VE for the image's size, and pveproxy fetches that from the
  # Image Factory inside one of its workers, which serves nothing else meanwhile and then drops every
  # connection it held, a request already sent on one included. The provider sends only reads again,
  # so the module's other changes to Proxmox VE are made before.
  depends_on = [
    proxmox_acl.cluster_api,
    proxmox_acl.clusters,
    proxmox_acl.clusters_audit,
    proxmox_acl.etcd_restore,
    proxmox_acl.ha,
    proxmox_acl.provisioner,
    proxmox_acl.template_builder,
    proxmox_user_token.cluster_api,
    proxmox_user_token.etcd_restore,
    proxmox_user_token.ha,
    proxmox_user_token.provisioner,
    proxmox_user_token.template_builder,
    proxmox_virtual_environment_pool.management,
  ]
}

resource "proxmox_virtual_environment_vm" "node" {
  count = length(local.nodes)

  name        = local.nodes[count.index].name
  description = "Kubehenge management cluster ${var.cluster_name}, managed by Terraform/OpenTofu"
  tags        = ["kubehenge", "management"]
  node_name   = local.nodes[count.index].proxmox_node
  pool_id     = proxmox_virtual_environment_pool.management.pool_id
  # A VM given another node moves there rather than being made again, which would take its etcd
  # member with it.
  migrate = true

  machine         = "q35"
  scsi_hardware   = "virtio-scsi-single"
  on_boot         = true
  stop_on_destroy = true
  # Rebooting on a hardware change would take every node down at once; a change of cores or
  # memory takes effect when each node next reboots, as a Talos upgrade reboots them one by one.
  reboot_after_update = false

  agent {
    enabled = true
  }

  cpu {
    cores = var.node_cores
    type  = var.cpu_type
  }

  memory {
    dedicated = var.node_memory_mb
  }

  disk {
    datastore_id = var.vm_datastore
    interface    = "scsi0"
    import_from  = proxmox_download_file.talos[local.nodes[count.index].proxmox_node].id
    size         = var.node_disk_gb
    discard      = "on"
    iothread     = true
    ssd          = true
  }

  network_device {
    bridge      = var.bridge
    vlan_id     = var.vlan_id
    model       = "virtio"
    mac_address = local.nodes[count.index].mac
  }

  # Talos reads its address from the NoCloud drive, so the nodes need no DHCP.
  initialization {
    datastore_id = var.vm_datastore

    ip_config {
      ipv4 {
        address = local.nodes[count.index].address
        gateway = var.gateway
      }
    }

    dns {
      servers = var.dns_servers
    }
  }

  operating_system {
    type = "l26"
  }

  serial_device {}

  lifecycle {
    # Talos upgrades nodes in place; a newer image is only for nodes added later.
    ignore_changes = [disk[0].import_from]

    precondition {
      condition     = length(local.address_overlaps) == 0
      error_message = "Pod or service ranges overlap the nodes' network or each other: ${join("; ", local.address_overlaps)}. Set pod_subnets and service_subnets to ranges the network does not use."
    }
  }
}
