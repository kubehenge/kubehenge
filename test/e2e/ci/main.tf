# The management cluster on the one-node Proxmox VE test/e2e/ci/up.sh starts. run.sh copies this
# beside its state and sets the variables.

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

variable "node_count" {
  type    = number
  default = 1
}

variable "node_cores" {
  type    = number
  default = 4
}

variable "version_overrides" {
  type    = map(string)
  default = {}
}

module "management_cluster" {
  # The whole repository, as a published module source would fetch it: the module reads ../charts.
  source = "${var.repository}//management-cluster"

  proxmox_endpoint       = "https://172.21.99.2:8006"
  proxmox_api_token      = var.proxmox_api_token
  proxmox_ca_certificate = var.proxmox_ca_certificate
  proxmox_nodes          = ["pve"]

  # The container's one storage, a directory.
  image_datastore     = "local"
  vm_datastore        = "local"
  bootstrap_datastore = "local"
  workload_datastores = ["local"]

  node_addresses = [for i in range(var.node_count) : "172.20.99.${11 + i}/24"]
  node_cores     = var.node_cores
  node_memory_mb = 3072
  # As the templates', test/e2e/ci/run.sh.
  cpu_type = "host"

  version_overrides = var.version_overrides

  api_address = "172.20.99.10"
  gateway     = "172.20.99.1"
  dns_servers = ["1.1.1.1", "9.9.9.9"]

  # The controller and Kubehenge's provider builds are pushed under their ghcr.io paths; everything
  # else falls through to ghcr.io.
  registry_mirrors = { "ghcr.io" = ["http://172.21.99.3:5000"] }
  controller_image = var.controller_image
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
