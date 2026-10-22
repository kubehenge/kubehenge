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

variable "proxmox_insecure" {
  description = "Skip verifying the Proxmox VE API's certificate: in this configuration's Proxmox VE provider, Cluster API's Proxmox VE provider, Kubehenge's controller and the clusters' cloud controller manager and CSI plugin."
  type        = bool
  default     = false
  nullable    = false
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
      "talos", "kubernetes", "cluster_api", "capmox",
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
  description = "Image registry mirrors for the management cluster's nodes, by registry name (such as ghcr.io or docker.io): endpoint URLs tried in order before the registry itself. A mirror of factory.talos.dev, ghcr.io or registry.k8s.io serves the images' cosign signatures too, which the nodes require on Sidero's and Kubernetes' images."
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

variable "backup_targets" {
  description = "S3 stores workload clusters back up to with Velero, when they switch backups on: each cluster writes under <prefix>/<namespace>.<cluster>/velero in the bucket, with keys of its own, its objects encrypted with a key of its own (SSE-C). endpoint is the store's https:// URL, null for AWS S3 itself; ca_certificate the PEM CA of its certificate when the system's do not cover it; checksums false for a store that refuses the checksums the AWS SDK sends."
  type = list(object({
    name           = string
    endpoint       = optional(string)
    bucket         = string
    region         = optional(string)
    prefix         = optional(string)
    ca_certificate = optional(string)
    checksums      = optional(bool, true)
  }))
  default  = []
  nullable = false

  validation {
    condition = length(var.backup_targets) <= 16 && length(distinct([for t in var.backup_targets : t.name])) == length(var.backup_targets) && alltrue([
      for t in var.backup_targets : can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", t.name)) && t.bucket != "" && (t.endpoint == null || startswith(coalesce(t.endpoint, "-"), "https://"))
    ])
    error_message = "At most 16 targets, each with its own name of lower-case letters, digits and dashes, a bucket, and an https:// endpoint or none."
  }
}

variable "proxmox_ha" {
  description = "Have Proxmox VE HA keep each cluster's control-plane VMs on separate nodes and restart them on another node when theirs fails. Kubehenge's controller gets a second API token for it, with Sys.Console on /, which amounts to a root shell on every node. Only clusters whose disks are on shared storage take part, and each cluster can opt out. Proxmox VE arms its watchdog on every node that runs an HA resource, so such a node that loses quorum restarts itself."
  type        = bool
  default     = false
  nullable    = false
}

variable "offer_ha_rules" {
  description = "Offer Proxmox VE's HA node-affinity rules as places for a cluster's VMs to those who may create or change a cluster but do not hold Sys.Audit on / themselves. Kubehenge's controller reads the rules with its own token, which holds Sys.Audit on / already, so nothing new is granted in Proxmox VE; the interface hands out each rule's name and the nodes it allows, never its comment or resources, and only to those with VM.Allocate on the workload pool or a cluster's pool. Every such user then learns the rules' names and which nodes they cover."
  type        = bool
  default     = false
  nullable    = false
}

variable "cluster_datastore_allocate" {
  description = "Let each cluster choose the Proxmox VE CSI plugin's newest release, which needs Datastore.Allocate on the cluster's storage: Kubehenge's controller then grants that cluster's token Datastore.Allocate there, which lets it delete any volume on that storage and change the storage's definition. A cluster may choose it only on a workload storage that neither the management cluster (vm_datastore, image_datastore, bootstrap_datastore, and with template_refresh its import_datastore and iso_datastore), its templates nor another cluster uses. Off, every cluster runs the plugin's last release that needs no more than Datastore.AllocateSpace and Datastore.Audit."
  type        = bool
  default     = false
  nullable    = false
}

variable "template_refresh" {
  description = <<-EOT
    Build the Ubuntu VM templates again every week, with the updates Ubuntu published since, and move each cluster's nodes to the new build inside its maintenance window once the build is soak_hours old. The two newest builds of each template stay, and any older one a cluster still clones from. The builds run in the management cluster with an API token of their own, which may create and delete VMs in the template pool, download to import_datastore and write to iso_datastore; it gets full control of those two storages (Proxmox VE deletes a file there only for Datastore.Allocate), so dedicated ones are best. The node defaults to the first of proxmox_nodes, storage to the first of workload_datastores, import_datastore to image_datastore and iso_datastore to bootstrap_datastore. The VM that prepares an image attaches to bridge (default the first of workload_bridges) and takes address (with prefix, gateway and dns), or DHCP when address is null. creator_builds lets those who may create clusters have a missing template built, such as for a Kubernetes version no kubeadm template exists for yet.
  EOT
  type = object({
    enabled          = optional(bool, true)
    weekday          = optional(string, "Sun")
    time             = optional(string, "01:00")
    time_zone        = optional(string, "UTC")
    soak_hours       = optional(number, 24)
    node             = optional(string)
    storage          = optional(string)
    import_datastore = optional(string)
    iso_datastore    = optional(string)
    bridge           = optional(string)
    vlan             = optional(number, 0)
    address          = optional(string)
    gateway          = optional(string)
    dns              = optional(list(string), [])
    creator_builds   = optional(bool, false)
  })
  default  = {}
  nullable = false

  validation {
    condition     = contains(["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"], var.template_refresh.weekday)
    error_message = "template_refresh.weekday is one of Mon, Tue, Wed, Thu, Fri, Sat and Sun."
  }
  validation {
    condition     = can(regex("^([01][0-9]|2[0-3]):[0-5][0-9]$", var.template_refresh.time))
    error_message = "template_refresh.time is HH:MM."
  }
  validation {
    condition     = var.template_refresh.soak_hours >= 0 && var.template_refresh.soak_hours <= 720 && floor(var.template_refresh.soak_hours) == var.template_refresh.soak_hours
    error_message = "template_refresh.soak_hours is a whole number of hours from 0 to 720."
  }
  validation {
    condition     = var.template_refresh.vlan >= 0 && var.template_refresh.vlan <= 4094
    error_message = "template_refresh.vlan is 0 for none, or a VLAN tag up to 4094."
  }
  validation {
    condition     = var.template_refresh.address == null ? var.template_refresh.gateway == null : can(cidrhost(var.template_refresh.address, 0)) && var.template_refresh.gateway != null
    error_message = "template_refresh.address is an address with prefix, such as 192.0.2.50/24, and needs a gateway; without it, the VM takes DHCP and gateway stays null."
  }
}

variable "cluster_sys_audit" {
  description = "Let each cluster choose the cloud controller manager's HA group labels, which need Sys.Audit on /: Kubehenge's controller then adds that cluster's user to a group holding Sys.Audit on /, unpropagated, so that its token can list Proxmox VE's HA groups and rules and read the rest of the Proxmox VE cluster's own configuration, but nothing of its guests, storages, nodes or users. Off, no cluster's token holds Sys.Audit on / and nodes go without the labels."
  type        = bool
  default     = false
  nullable    = false
}

variable "cluster_volume_snapshots" {
  description = "Let each cluster choose volume snapshots, restores and clones of its persistent volumes. The Proxmox VE CSI plugin copies volumes through a method only root@pam may call (no API token passes), so the CSI plugin of a cluster that chooses them signs in to Proxmox VE as root@pam, and whoever takes over that cluster or reads its Proxmox VE Secret holds the whole Proxmox VE cluster. The password is no variable: Kubehenge's controller reads it from the Secret kubehenge-root-pam (key password) in the kubehenge-system namespace, which Kubehenge's appliance writes; root@pam must have no second factor. Only on storage local to each node (ZFS, LVM, LVM-thin, directory or btrfs)."
  type        = bool
  default     = false
  nullable    = false
}

variable "extra_cluster_namespaces" {
  description = "More namespaces of the management cluster for workload clusters, beside clusters: the web interface creates, changes and hands out kubeconfigs of clusters in each, with the same rights it has in clusters, and lets whoever creates a cluster choose among them. The namespaces are created when missing and kept with their clusters when taken out of the list; the interface's rights there go. None may be one of Kubernetes', the providers' or Kubehenge's own: default, kube-*, *-system or cert-manager."
  type        = list(string)
  default     = []
  nullable    = false

  validation {
    condition = (
      length(distinct(var.extra_cluster_namespaces)) == length(var.extra_cluster_namespaces) &&
      alltrue([for n in var.extra_cluster_namespaces : (
        can(regex("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$", n)) &&
        !contains(["clusters", "default", "cert-manager"], n) &&
        !startswith(n, "kube-") && !endswith(n, "-system")
      )])
    )
    error_message = "extra_cluster_namespaces names each namespace once, as a DNS label, and none of clusters, default, kube-*, *-system or cert-manager."
  }
}

variable "etcd_restore" {
  description = "Have Kubehenge's controller restore clusters' etcd from their snapshots itself, running the restore on the control-plane VMs through their guest agents. The controller gets an API token of its own for it, with VM.Audit and VM.GuestAgent.Unrestricted on the workload pool, which amounts to a root shell in every cluster's VMs. Without it, the interface shows the steps to restore by hand."
  type        = bool
  default     = false
  nullable    = false
}

variable "headlamp_sign_in" {
  description = "Have the appliance's shared Headlamp sign those who may download a cluster's admin kubeconfig (Sys.Modify on its pool) in as cluster-admin, with a token made for their session that never reaches the browser. Kubehenge's controller then keeps, in every cluster of clusters and extra_cluster_namespaces, an identity bound to cluster-admin and one that may only make its tokens, whose own token it keeps beside the Cluster for the interface: whoever reads Secrets there, as the interface does, can act as cluster-admin in those clusters, as the admin kubeconfigs beside them already let them. Set back to false, the controller removes both, which ends every token made."
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
