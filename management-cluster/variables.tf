variable "proxmox_endpoint" {
  description = "URL of the Proxmox VE API, such as https://pve.example.org:8006."
  type        = string
}

variable "proxmox_api_token" {
  description = "API token this configuration signs in with, as user@realm!name=secret. It creates users, roles, pools and VMs, so it needs administrator rights. Null signs in with a user's session instead, from PROXMOX_VE_AUTH_TICKET and PROXMOX_VE_CSRF_PREVENTION_TOKEN, as Kubehenge's appliance does; a session lasts two hours, so an apply has to finish within that."
  type        = string
  sensitive   = true
  default     = null
}

variable "proxmox_ca_certificate" {
  description = "PEM certificate of the CA that signed the Proxmox VE API's certificate: /etc/pve/pve-root-ca.pem on a Proxmox VE node unless the certificate was replaced. Cluster API's Proxmox VE provider verifies the API against it."
  type        = string

  validation {
    condition     = can(regex("-----BEGIN CERTIFICATE-----", var.proxmox_ca_certificate))
    error_message = "proxmox_ca_certificate must be a PEM certificate."
  }
}

variable "proxmox_nodes" {
  description = "Proxmox VE nodes the management cluster's VMs run on, taken in turn: the first VM on the first node, the second on the second, and so on. Three nodes for three VMs keep the management cluster running when one node fails. Each node needs the bridge, vm_datastore and image_datastore."
  type        = list(string)

  validation {
    condition     = length(var.proxmox_nodes) > 0 && length(distinct(var.proxmox_nodes)) == length(var.proxmox_nodes)
    error_message = "proxmox_nodes names at least one node, each once."
  }
}

variable "image_datastore" {
  description = "Storage the Talos disk image is downloaded to. It must allow the `import` content type."
  type        = string
  default     = "local"
}

variable "vm_datastore" {
  description = "Storage for the management cluster's VM disks."
  type        = string
  default     = "local-lvm"
}

variable "bridge" {
  description = "Bridge the management cluster's VMs attach to."
  type        = string
  default     = "vmbr0"
}

variable "vlan_id" {
  description = "VLAN tag for the management cluster's VMs, or null for none."
  type        = number
  default     = null
}

variable "cluster_name" {
  description = "Name of the management cluster, also the prefix of its VM names."
  type        = string
  default     = "kubehenge"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$", var.cluster_name))
    error_message = "cluster_name must be a DNS label of at most 32 characters."
  }
}

variable "node_addresses" {
  description = "Static address of each control-plane node, with prefix length, such as [\"192.0.2.11/24\"]. One node, or three for high availability. Once the cluster runs, only append: a node given another address loses its etcd member, and one removed leaves it behind."
  type        = list(string)

  validation {
    condition     = contains([1, 3, 5], length(var.node_addresses)) && alltrue([for a in var.node_addresses : can(cidrhost(a, 0))])
    error_message = "node_addresses must hold one, three or five addresses with prefix length."
  }
}

variable "api_address" {
  description = "Virtual IP of the Kubernetes API, in the nodes' subnet and not used by anything else. It moves to a healthy control-plane node if one fails."
  type        = string

  validation {
    condition     = can(cidrhost("${var.api_address}/32", 0))
    error_message = "api_address must be an IPv4 address without prefix length."
  }
}

variable "gateway" {
  description = "Default gateway of the nodes' subnet."
  type        = string
}

variable "dns_servers" {
  description = "DNS servers the nodes use."
  type        = list(string)
}

variable "version_overrides" {
  description = "Versions to use instead of the tested set in versions.tf, by the same keys, for holding a component back. A provider held back needs its image in provider_images and its files' digests in provider_manifests. Lowering a version on a running cluster is not supported."
  type        = map(string)
  default     = {}

  validation {
    condition = alltrue([for k in keys(var.version_overrides) : contains([
      "talos", "kubernetes", "cert_manager", "capi_operator", "cluster_api", "capmox",
      "rke2", "k3s", "ipam_in_cluster", "helm_addon",
    ], k)])
    error_message = "version_overrides takes only the keys of the tested set in versions.tf."
  }
}

variable "provider_images" {
  description = "Controller images by digest (repository@sha256:<digest>) to run instead of the tested ones, by \"<kind>/<name>\" (for example \"InfrastructureProvider/proxmox\"): a registry mirror, a build carrying a fix not yet released, or the image of a version held back in version_overrides, which needs one. The release's manifests still come from its version."
  type        = map(string)
  default     = {}

  validation {
    condition = alltrue([for k in keys(var.provider_images) : contains([
      "CoreProvider/cluster-api",
      "BootstrapProvider/kubeadm", "BootstrapProvider/rke2", "BootstrapProvider/k3s",
      "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/rke2", "ControlPlaneProvider/k3s",
      "InfrastructureProvider/proxmox", "IPAMProvider/in-cluster", "AddonProvider/helm",
    ], k)])
    error_message = "provider_images takes only the providers the management cluster installs, as \"<kind>/<name>\"."
  }
  validation {
    condition     = alltrue([for image in values(var.provider_images) : can(regex("^[^@]+@sha256:[0-9a-f]{64}$", image))])
    error_message = "provider_images takes each image by digest, repository@sha256:<digest>."
  }
}

variable "provider_manifests" {
  description = "The SHA-256 of provider files outside the tested set, by their address under https://github.com/ (for example \"kubernetes-sigs/cluster-api/releases/download/v1.14.2/core-components.yaml\"): a version held back in version_overrides needs its metadata.yaml and components file's."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for path, digest in var.provider_manifests : can(regex("^[^/]+/[^/]+/releases/download/[^/]+/[^/]+\\.yaml$", path)) && can(regex("^[0-9a-f]{64}$", digest))])
    error_message = "provider_manifests maps <owner>/<repository>/releases/download/<version>/<file>.yaml to its SHA-256 in hex."
  }
}

variable "pod_subnets" {
  description = "Address ranges of the management cluster's pods. They must not overlap the nodes' network."
  type        = list(string)
  default     = ["10.244.0.0/16"]
}

variable "service_subnets" {
  description = "Address ranges of the management cluster's services. Kubernetes' default, 10.96.0.0/12, spans 10.96.0.0 to 10.111.255.255; choose another if the nodes' network is in it."
  type        = list(string)
  default     = ["10.96.0.0/12"]
}

variable "node_cores" {
  description = "vCPUs per control-plane node. With two, the API server falls behind when the Cluster API providers all restart together, as an upgrade makes them, and their health checks start failing."
  type        = number
  default     = 4
}

variable "node_memory_mb" {
  description = "Memory per control-plane node, in MiB."
  type        = number
  default     = 4096
}

variable "node_disk_gb" {
  description = "System disk size per control-plane node, in GB."
  type        = number
  default     = 32
}

variable "cpu_type" {
  description = "QEMU CPU type of the nodes. Talos needs at least x86-64-v2."
  type        = string
  default     = "x86-64-v2-AES"
}

variable "workload_pool" {
  description = "Proxmox VE pool that holds the VMs of the clusters Kubehenge creates, each cluster in a pool of its own nested under it. Cluster API may manage only what is in it."
  type        = string
  default     = "kubehenge"

  validation {
    # Proxmox VE nests pools three levels deep; each cluster's pool takes one.
    condition     = can(regex("^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$", var.workload_pool))
    error_message = "The workload pool is a Proxmox VE pool name, nested at most one level (parent/child)."
  }
}

variable "template_pool" {
  description = "Proxmox VE pool that holds the VM templates clusters are cloned from."
  type        = string
  default     = "kubehenge-templates"
}

variable "management_pool" {
  description = "Proxmox VE pool that holds the management cluster's own VMs, out of Cluster API's reach."
  type        = string
  default     = "kubehenge-management"
}

variable "workload_datastores" {
  description = "Storages that may hold the disks of the clusters Kubehenge creates."
  type        = list(string)
  default     = ["local-lvm"]
}

variable "workload_bridges" {
  description = "Bridges the clusters Kubehenge creates may attach to, with any VLAN on them."
  type        = list(string)
  default     = ["vmbr0"]
}

variable "bootstrap_datastore" {
  description = "Storage where Cluster API keeps each new VM's cloud-init ISO. It gets full control of this storage, so a dedicated one is best."
  type        = string
  default     = "local"
}

variable "registry_mirrors" {
  description = "Image registry mirrors for the management cluster's nodes, by registry name (such as ghcr.io or docker.io): endpoint URLs tried in order before the registry itself."
  type        = map(list(string))
  default     = {}
}

variable "cluster_oidc" {
  description = "OpenID Connect providers every new workload cluster's API server trusts unless the cluster names its own; tokens and certificates keep working beside them. Headlamp and kubectl sign in with the first."
  type = list(object({
    issuer_url            = string
    client_id             = string
    username_claim        = optional(string, "email")
    username_prefix       = optional(string, "oidc:")
    groups_claim          = optional(string, "groups")
    groups_prefix         = optional(string, "oidc:")
    scopes                = optional(list(string), ["email", "profile"])
    certificate_authority = optional(string)
  }))
  default  = []
  nullable = false

  validation {
    condition     = length(var.cluster_oidc) <= 8 && alltrue([for p in var.cluster_oidc : startswith(p.issuer_url, "https://") && p.client_id != ""])
    error_message = "At most eight providers, each with an https:// issuer_url and a client_id."
  }
}

variable "proxmox_ha" {
  description = "Have Proxmox VE HA keep each cluster's control-plane VMs on separate nodes and restart them on another node when theirs fails. Kubehenge's controller gets a second API token for it, with Sys.Console on /, which amounts to a root shell on every node. Only clusters whose disks are on shared storage take part, and each cluster can opt out. Proxmox VE arms its watchdog on every node that runs an HA resource, so such a node that loses quorum restarts itself."
  type        = bool
  default     = false
  nullable    = false
}

variable "controller_image" {
  description = "Image of Kubehenge's controller by digest, as repository@sha256:<digest>, instead of the one the chart was released with."
  type        = string
  default     = null

  validation {
    condition     = var.controller_image == null || can(regex("^[^@]+@sha256:[0-9a-f]{64}$", var.controller_image))
    error_message = "controller_image is the image by digest, repository@sha256:<digest>."
  }
}
