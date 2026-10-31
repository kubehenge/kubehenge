# The management cluster on the Proxmox VE test/e2e/ci/up.sh starts, one node or two. run.sh copies
# this beside its state and sets the variables.

variable "proxmox_api_token" {
  type      = string
  sensitive = true
}

variable "proxmox_ca_certificate" {
  type = string
}

# The checkout under test; OpenTofu reads it at init to find the module.
variable "repository" {
  type = string
}

# The controller built from the tree under test, pushed to the run's registry, by digest.
variable "controller_image" {
  type = string
}

# The weekday of the scheduled template refresh, three days after the run's start: the tests start
# every run they look at themselves, and one the schedule started beside them would add builds of
# its own.
variable "refresh_weekday" {
  type = string
}

variable "node_count" {
  type    = number
  default = 1
}

# With a second node (test/e2e/ci/second-node.sh), the nodes and the storage both share.
variable "proxmox_nodes" {
  type    = list(string)
  default = ["pve"]
}

variable "datastore" {
  type    = string
  default = "local"
}

variable "node_cores" {
  type    = number
  default = 4
}

variable "node_memory_mb" {
  type    = number
  default = 4096
}

variable "version_overrides" {
  type    = map(string)
  default = {}
}

variable "provider_images" {
  type    = map(string)
  default = {}
}

variable "provider_manifests" {
  type    = map(string)
  default = {}
}

# The S3 store test/e2e/workload-cluster.sh serves the clusters' backups from.
variable "backup_targets" {
  type = list(object({
    name           = string
    endpoint       = optional(string)
    bucket         = string
    region         = optional(string)
    prefix         = optional(string)
    ca_certificate = optional(string)
    checksums      = optional(bool, true)
  }))
  default = []
}

module "management_cluster" {
  # The whole repository, as a published module source would fetch it: the module reads ../charts.
  source = "${var.repository}//management-cluster"

  proxmox_endpoint       = "https://172.21.99.2:8006"
  proxmox_api_token      = var.proxmox_api_token
  proxmox_ca_certificate = var.proxmox_ca_certificate
  proxmox_nodes          = var.proxmox_nodes

  # The container's storage, a directory, and another the k3s test's cluster holds
  # Datastore.Allocate on.
  image_datastore            = var.datastore
  vm_datastore               = var.datastore
  bootstrap_datastore        = var.datastore
  workload_datastores        = distinct([var.datastore, "local", "own"])
  cluster_datastore_allocate = true

  node_addresses = [for i in range(var.node_count) : "172.20.99.${11 + i}/24"]
  node_cores     = var.node_cores
  node_memory_mb = var.node_memory_mb
  # As the templates', test/e2e/ci/run.sh.
  cpu_type = "host"

  version_overrides  = var.version_overrides
  provider_images    = var.provider_images
  provider_manifests = var.provider_manifests
  backup_targets     = var.backup_targets
  # test/e2e/workload-cluster.sh restores each cluster's etcd through the guest agent.
  etcd_restore = true
  # The RKE2 and k3s tests hold their upgrade's first new server out long enough to see the roll
  # reported as stalled, and test/e2e/workload-cluster.sh holds a deletion up as long.
  watch = { stall_minutes = 2 }

  api_address = "172.20.99.10"
  gateway     = "172.20.99.1"
  dns_servers = ["172.20.99.1"]

  # The controller and Kubehenge's provider builds are pushed under their ghcr.io paths; everything
  # else falls through to ghcr.io.
  registry_mirrors = { "ghcr.io" = ["http://172.21.99.3:5000"] }
  controller_image = var.controller_image

  # No soak, so that the node image test sees a cluster move to a build as soon as it is made. Its
  # builds take an address apart from the one run.sh's take, so that the two never meet.
  template_refresh = {
    weekday        = var.refresh_weekday
    soak_hours     = 0
    address        = "172.20.99.51/24"
    gateway        = "172.20.99.1"
    dns            = ["172.20.99.1"]
    creator_builds = true
  }

  # The k3s test's cluster is created in it with the interface's account (test/e2e/ci/run.sh).
  extra_cluster_namespaces = ["e2e-team"]

  # Checked by the workload cluster tests: the k3s test's cluster, in one of the interface's
  # namespaces, gets the sign-in identities, and the others' clusters, in namespaces of their own, do
  # not.
  headlamp_sign_in = true
}

output "kubeconfig" {
  value     = module.management_cluster.kubeconfig
  sensitive = true
}

output "talosconfig" {
  value     = module.management_cluster.talosconfig
  sensitive = true
}

output "proxmox_objects" {
  value = module.management_cluster.proxmox_objects
}
