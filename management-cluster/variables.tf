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
  description = "Proxmox VE nodes the management cluster's VMs run on, taken in turn: the first VM on the first node, the second on the second, and so on. Three nodes for three VMs keep the management cluster running when one node fails. Each node needs the bridge, vm_datastore and image_datastore. Once the VMs exist, a change of the list migrates each VM whose turn falls on another node."
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
  description = "Bridge the management cluster's VMs attach to: a node's bridge, such as vmbr0, or an SDN VNet as zone/vnet, such as evpn1/web."
  type        = string
  default     = "vmbr0"

  validation {
    condition     = can(regex("^([A-Za-z][A-Za-z0-9-]*/)?[A-Za-z0-9][A-Za-z0-9._-]*$", var.bridge))
    error_message = "bridge is a node's bridge, such as vmbr0, or a VNet as zone/vnet, such as evpn1/web."
  }
}

variable "vlan_id" {
  description = "VLAN tag for the management cluster's VMs, or null for none."
  type        = number
  default     = null
}

variable "mtu" {
  description = "MTU of the management cluster's VMs' network interface, 1280 to 65520, for a network that carries less than 1500 (VXLAN underlays) or more (jumbo frames). It cannot exceed the bridge's. Null keeps the bridge's."
  type        = number
  default     = null

  validation {
    condition     = var.mtu == null ? true : var.mtu >= 1280 && var.mtu <= 65520 && var.mtu == floor(var.mtu)
    error_message = "mtu is a whole number from 1280 to 65520, or null for the bridge's."
  }
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

variable "ip_families" {
  description = "The cluster's address families: \"IPv4\", \"IPv6\" or both. The first is the primary one, which the API address, the nodes' Talos and etcd traffic and the API endpoint use. Both make a dual-stack cluster, which needs node_addresses and node_addresses_ipv6 of the same length."
  type        = list(string)
  default     = ["IPv4"]

  validation {
    condition     = contains([1, 2], length(var.ip_families)) && length(distinct(var.ip_families)) == length(var.ip_families) && alltrue([for f in var.ip_families : contains(["IPv4", "IPv6"], f)])
    error_message = "ip_families is [\"IPv4\"], [\"IPv6\"], [\"IPv4\", \"IPv6\"] or [\"IPv6\", \"IPv4\"]."
  }
}

variable "node_addresses" {
  description = "Static IPv4 address of each control-plane node, with prefix length, such as [\"192.0.2.11/24\"]. Needed when ip_families holds IPv4, and an empty list otherwise. One node, or three for high availability. Once the cluster runs, only append: a node given another address loses its etcd member, and one removed leaves it behind."
  type        = list(string)

  validation {
    condition = contains(var.ip_families, "IPv4") ? (
      contains([1, 3, 5], length(var.node_addresses)) && alltrue([for a in var.node_addresses : can(cidrhost(a, 0)) && !strcontains(a, ":")])
    ) : length(var.node_addresses) == 0
    error_message = "node_addresses must hold one, three or five IPv4 addresses with prefix length when ip_families holds IPv4, and none otherwise."
  }
}

variable "node_addresses_ipv6" {
  description = "Static IPv6 address of each control-plane node, with prefix length, such as [\"2001:db8::11/64\"]. Needed when ip_families holds IPv6, and empty otherwise. A dual-stack cluster gives as many as node_addresses, and node i takes element i of each."
  type        = list(string)
  default     = []

  validation {
    condition = contains(var.ip_families, "IPv6") ? (
      contains([1, 3, 5], length(var.node_addresses_ipv6)) && alltrue([for a in var.node_addresses_ipv6 : can(cidrhost(a, 0)) && strcontains(a, ":")])
    ) : length(var.node_addresses_ipv6) == 0
    error_message = "node_addresses_ipv6 must hold one, three or five IPv6 addresses with prefix length when ip_families holds IPv6, and none otherwise."
  }

  validation {
    condition     = length(var.ip_families) < 2 || length(var.node_addresses) == length(var.node_addresses_ipv6)
    error_message = "A dual-stack cluster needs as many node_addresses_ipv6 as node_addresses."
  }
}

variable "appliance_addresses" {
  description = "Addresses without prefix length that Kubehenge's appliance reaches the workload clusters' API servers from, for its interface and its Headlamp. A workload cluster with firewall security groups accepts them, as it does the management cluster's nodes. The appliance sets its own."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for a in var.appliance_addresses : can(cidrhost("${a}/${strcontains(a, ":") ? 128 : 32}", 0))])
    error_message = "appliance_addresses must hold IP addresses without prefix length."
  }
}

variable "api_address" {
  description = "Virtual IP of the Kubernetes API, an address of the primary family without prefix length, in that family's node network and used by nothing else. It moves to a healthy control-plane node if one fails."
  type        = string

  validation {
    condition     = can(cidrhost("${var.api_address}/${var.ip_families[0] == "IPv6" ? 128 : 32}", 0)) && strcontains(var.api_address, ":") == (var.ip_families[0] == "IPv6")
    error_message = "api_address must be an address of the primary family in ip_families, without prefix length."
  }

  validation {
    condition = try(
      cidrsubnet(var.ip_families[0] == "IPv6" ? var.node_addresses_ipv6[0] : var.node_addresses[0], 0, 0) ==
      cidrsubnet("${var.api_address}/${split("/", var.ip_families[0] == "IPv6" ? var.node_addresses_ipv6[0] : var.node_addresses[0])[1]}", 0, 0),
      true
    )
    error_message = "api_address must be inside the nodes' network of the primary family."
  }

  validation {
    condition = try(
      !contains(
        [for a in(var.ip_families[0] == "IPv6" ? var.node_addresses_ipv6 : var.node_addresses) : cidrhost("${split("/", a)[0]}/${var.ip_families[0] == "IPv6" ? 128 : 32}", 0)],
        cidrhost("${var.api_address}/${var.ip_families[0] == "IPv6" ? 128 : 32}", 0)
      ) && cidrhost("${var.api_address}/${var.ip_families[0] == "IPv6" ? 128 : 32}", 0) != cidrhost("${var.ip_families[0] == "IPv6" ? var.gateway_ipv6 : var.gateway}/${var.ip_families[0] == "IPv6" ? 128 : 32}", 0),
      true
    )
    error_message = "api_address must be neither a node address nor the gateway."
  }
}

variable "gateway" {
  description = "Default IPv4 gateway of the nodes' network. Needed when ip_families holds IPv4, and unset otherwise."
  type        = string
  default     = null

  validation {
    condition = contains(var.ip_families, "IPv4") ? (
      var.gateway == null ? false : can(cidrhost("${var.gateway}/32", 0)) && !strcontains(var.gateway, ":")
    ) : var.gateway == null
    error_message = "gateway must be an IPv4 address when ip_families holds IPv4, and unset otherwise."
  }
}

variable "gateway_ipv6" {
  description = "Default IPv6 gateway of the nodes' network. Needed when ip_families holds IPv6, and unset otherwise."
  type        = string
  default     = null

  validation {
    condition = contains(var.ip_families, "IPv6") ? (
      var.gateway_ipv6 == null ? false : can(cidrhost("${var.gateway_ipv6}/128", 0)) && strcontains(var.gateway_ipv6, ":")
    ) : var.gateway_ipv6 == null
    error_message = "gateway_ipv6 must be an IPv6 address when ip_families holds IPv6, and unset otherwise."
  }
}

variable "dns_servers" {
  description = "DNS servers the nodes use, addresses of the cluster's families. A dual-stack cluster needs at least one of each."
  type        = list(string)

  validation {
    condition = alltrue([
      for d in var.dns_servers : can(cidrhost("${d}/${strcontains(d, ":") ? 128 : 32}", 0)) && contains(var.ip_families, strcontains(d, ":") ? "IPv6" : "IPv4")
    ])
    error_message = "dns_servers must be addresses of the families in ip_families."
  }

  validation {
    condition     = length(var.ip_families) < 2 || alltrue([for f in var.ip_families : anytrue([for d in var.dns_servers : strcontains(d, ":") == (f == "IPv6")])])
    error_message = "A dual-stack cluster needs at least one IPv4 and one IPv6 DNS server."
  }
}

variable "search_domains" {
  description = "Domains the nodes append to a name without dots, as DNS names. Empty derives one from each node's host name, as Talos does."
  type        = list(string)
  default     = []

  validation {
    # glibc's resolver reads six. The pattern is the one of the clusters' network.searchDomains.
    condition = length(var.search_domains) <= 6 && alltrue([
      for d in var.search_domains : length(d) <= 253 && can(regex("^([A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?\\.)*[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?$", d))
    ])
    error_message = "search_domains are at most six DNS names, without a trailing dot."
  }
}

variable "time_servers" {
  description = "NTP servers the management cluster's nodes keep their clocks with, as host names or addresses. Empty keeps Talos's default public servers."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for s in var.time_servers : trimspace(s) != "" && !strcontains(s, "/") && !strcontains(s, " ")])
    error_message = "time_servers are host names or addresses, without a scheme, path or space."
  }
}

variable "version_overrides" {
  description = "Versions to use instead of the tested set in versions.tf, by the same keys, for holding a component back. A provider held back needs its image in provider_images and its files' digests in provider_manifests. Lowering a version on a running cluster is not supported."
  type        = map(string)
  default     = {}

  validation {
    condition = alltrue([for k in keys(var.version_overrides) : contains([
      "talos", "kubernetes", "cluster_api", "capmox",
      "rke2", "k3s", "ipam_in_cluster", "helm_addon", "talos_bootstrap", "talos_control_plane",
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
      "BootstrapProvider/kubeadm", "BootstrapProvider/rke2", "BootstrapProvider/k3s", "BootstrapProvider/talos",
      "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/rke2", "ControlPlaneProvider/k3s", "ControlPlaneProvider/talos",
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
  description = "Address ranges of the management cluster's pods, one per family in ip_families order. They must not overlap the nodes' network. Unset takes 10.244.0.0/16 for IPv4 and fd5e:4b1c:9a20::/56 for IPv6. An IPv6 range is /48 to /63, since each node takes a /64 of it and Kubernetes allows 16 bits between."
  type        = list(string)
  default     = null

  validation {
    condition = var.pod_subnets == null ? true : try(
      length(var.pod_subnets) == length(var.ip_families) && alltrue([
        for i, c in var.pod_subnets : can(cidrhost(c, 0)) && strcontains(c, ":") == (var.ip_families[i] == "IPv6") &&
        cidrhost(c, 0) == cidrhost("${split("/", c)[0]}/${strcontains(c, ":") ? 128 : 32}", 0)
      ]),
      false
    )
    error_message = "pod_subnets holds one network without host bits per family in ip_families, in that order."
  }

  validation {
    condition     = var.pod_subnets == null ? true : try(alltrue([for c in var.pod_subnets : !strcontains(c, ":") || (tonumber(split("/", c)[1]) >= 48 && tonumber(split("/", c)[1]) <= 63)]), true)
    error_message = "An IPv6 pod range is /48 to /63."
  }
}

variable "service_subnets" {
  description = "Address ranges of the management cluster's services, one per family in ip_families order. Unset takes 10.96.0.0/12 for IPv4, which spans 10.96.0.0 to 10.111.255.255, and fd5e:4b1c:9a21::/112 for IPv6. Choose another if the nodes' network is in it. An IPv6 range is /108 to /120."
  type        = list(string)
  default     = null

  validation {
    condition = var.service_subnets == null ? true : try(
      length(var.service_subnets) == length(var.ip_families) && alltrue([
        for i, c in var.service_subnets : can(cidrhost(c, 0)) && strcontains(c, ":") == (var.ip_families[i] == "IPv6") &&
        cidrhost(c, 0) == cidrhost("${split("/", c)[0]}/${strcontains(c, ":") ? 128 : 32}", 0)
      ]),
      false
    )
    error_message = "service_subnets holds one network without host bits per family in ip_families, in that order."
  }

  validation {
    condition     = var.service_subnets == null ? true : try(alltrue([for c in var.service_subnets : !strcontains(c, ":") || (tonumber(split("/", c)[1]) >= 108 && tonumber(split("/", c)[1]) <= 120)]), true)
    error_message = "An IPv6 service range is /108 to /120."
  }
}

variable "node_cores" {
  description = "vCPUs per control-plane node. With two, the API server falls behind when the Cluster API providers all restart together, as an upgrade makes them, and their health checks start failing."
  type        = number
  default     = 4
}

variable "node_cpu_units" {
  description = "Proxmox VE CPU weight of each control-plane node. When the host's CPUs are contended, a node gets this share against 100 for a VM at Proxmox VE's default, so the API server keeps answering. A change reaches running nodes at once, without a reboot."
  type        = number
  default     = 200

  validation {
    condition     = var.node_cpu_units >= 1 && var.node_cpu_units <= 10000 && floor(var.node_cpu_units) == var.node_cpu_units
    error_message = "node_cpu_units must be a whole number from 1 to 10000, the weights Proxmox VE keeps under cgroup v2."
  }
}

variable "node_memory_mb" {
  description = "Memory per control-plane node, in MiB."
  type        = number
  default     = 4096
}

variable "node_memory_min_mb" {
  description = "Least memory per control-plane node, in MiB, that Proxmox VE's ballooning takes a node down to while its host is above 80 % memory use. Unset, each node keeps node_memory_mb."
  type        = number
  default     = null

  validation {
    condition     = var.node_memory_min_mb == null || (var.node_memory_min_mb >= 2048 && var.node_memory_min_mb <= var.node_memory_mb)
    error_message = "node_memory_min_mb must be at least 2048, Talos's least for a control-plane node, and at most node_memory_mb."
  }
}

variable "node_disk_gb" {
  description = "System disk size per control-plane node, in GB."
  type        = number
  default     = 32
}

variable "vm_id_range" {
  description = "The range the control-plane VMs' IDs are drawn from at random: start is the lowest ID and end is one past the highest, as the provider draws them. Set it inside the datacenter's next-id range (Datacenter, Options) when administrators reserve ID bands. The VMs keep the IDs they have when it changes."
  type = object({
    start = number
    end   = number
  })
  default = {
    start = 10000
    end   = 99999
  }

  validation {
    condition = (
      floor(var.vm_id_range.start) == var.vm_id_range.start && floor(var.vm_id_range.end) == var.vm_id_range.end &&
      var.vm_id_range.start >= 100 && var.vm_id_range.start < var.vm_id_range.end && var.vm_id_range.end <= 1000000000
    )
    error_message = "vm_id_range must be whole numbers with start below end, start at least 100 and end at most 1000000000, since Proxmox VE's VM IDs run from 100 to 999999999."
  }
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
  description = "Bridges the clusters Kubehenge creates may attach to, with any VLAN on them (but none on a VNet of a VLAN zone, which tags already). A node's bridge is written by name, such as vmbr0, and gets PVESDNUser on /sdn/zones/localnetwork/<bridge>. An SDN VNet is written zone/vnet, such as evpn1/web, and gets it on /sdn/zones/<zone>/<vnet>."
  type        = list(string)
  default     = ["vmbr0"]

  validation {
    condition     = alltrue([for b in var.workload_bridges : can(regex("^([A-Za-z][A-Za-z0-9-]*/)?[A-Za-z0-9][A-Za-z0-9._-]*$", b))])
    error_message = "workload_bridges lists node bridges, such as vmbr0, and VNets as zone/vnet, such as evpn1/web."
  }
}

variable "bootstrap_datastore" {
  description = "Storage where Cluster API keeps each new VM's cloud-init ISO. It gets full control of this storage, so a dedicated one is best."
  type        = string
  default     = "local"
}

variable "registry_mirrors" {
  description = "Image registry mirrors for the management cluster's nodes, by registry name (such as ghcr.io or docker.io): endpoint URLs tried in order before the registry itself. A mirror of factory.talos.dev, ghcr.io or registry.k8s.io serves the images' cosign signatures too, which the nodes require on Sidero's and Kubernetes' images and on the Talos providers'."
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

variable "watch" {
  description = "How long Kubehenge's controller gives what it watches for before it reports it on the Cluster and the interface's Health page: stall_minutes, for the new machine in the roll of a cluster's only control plane to become ready, and for a cluster's deletion to finish."
  type = object({
    stall_minutes = optional(number, 20)
  })
  default  = {}
  nullable = false

  validation {
    condition     = var.watch.stall_minutes >= 1 && var.watch.stall_minutes <= 1440 && floor(var.watch.stall_minutes) == var.watch.stall_minutes
    error_message = "watch.stall_minutes is a whole number of minutes from 1 to 1440."
  }
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
    Build the node OS VM templates again every week, with the updates each node OS published since, and move each cluster's nodes to the new build inside its upgrade window once the build is soak_hours old. The two newest builds of each template stay, and any older one a cluster still clones from. The builds run in the management cluster with an API token of their own, which may create and delete VMs in the template pool, download to import_datastore and write to iso_datastore; it gets full control of those two storages (Proxmox VE deletes a file there only for Datastore.Allocate), so dedicated ones are best. The node defaults to the first of proxmox_nodes, storage to the first of workload_datastores, import_datastore to image_datastore and iso_datastore to bootstrap_datastore. The VM that builds a node image attaches to bridge (default the first of workload_bridges, a VNet written zone/vnet as there) and takes address (with prefix, gateway and dns), or DHCP when address is null. creator_builds lets those who may create clusters have a missing template built, such as for a Kubernetes version no kubeadm template exists for yet. kubeadm_patches also builds the kubeadm template of the newest patch release of each Kubernetes minor version a kubeadm cluster runs, and of the next minor version, so that patch releases reach kubeadm clusters; false switches that off.
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
    kubeadm_patches  = optional(bool, true)
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
  validation {
    condition     = var.template_refresh.bridge == null || can(regex("^([A-Za-z][A-Za-z0-9-]*/)?[A-Za-z0-9][A-Za-z0-9._-]*$", var.template_refresh.bridge))
    error_message = "template_refresh.bridge is a node's bridge, such as vmbr0, or a VNet as zone/vnet, such as evpn1/web."
  }
}

variable "cluster_sys_audit" {
  description = "Give each cluster the cloud controller manager's HA group labels unless the cluster switches them off. They need Sys.Audit on /: Kubehenge's controller adds the cluster's user to a group holding Sys.Audit on /, unpropagated, so that its token can list Proxmox VE's HA groups and rules and read the rest of the Proxmox VE cluster's own configuration (firewall rules, backup jobs, HA, the corosync configuration), but nothing of its guests, storages, nodes or users. Off, no cluster's token holds Sys.Audit on / and nodes go without the labels."
  type        = bool
  default     = true
  nullable    = false
}

variable "node_boot_logs" {
  description = "Let Kubehenge's controller read the boot logs of kubeadm, RKE2 and k3s nodes when asked, with its own token holding VM.GuestAgent.FileRead on the workload pool. Through the guest agent it can then read any file of a cluster's VMs that the agent's policy in the VM lets it open, which Kubehenge's node templates limit to cloud-init's output and the node's copy of its boot's journals; it runs no commands. Off, the token holds no guest agent privilege, and only Talos nodes' boot logs, read through their Talos API, are offered."
  type        = bool
  default     = true
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

variable "headlamp_actions" {
  description = "Have the appliance's shared Headlamp offer the Helm app catalog and the Prometheus charts it ships with: Helm releases listed, installed from repositories, upgraded, rolled back and uninstalled with each user's own credentials in the cluster. Only those who may set the appliance up add or remove a repository, since one list serves every user and cluster. Port forwards stay with Headlamp's desktop app, and node drain is offered either way."
  type        = bool
  default     = false
  nullable    = false
}

variable "api_audit_log" {
  description = "Have the management cluster's API server log each change a person makes, such as with kubectl, so the interface's activity page shows it with its user. The API server then logs only these changes, to its own output instead of a file on the node. The interface reads them as the API server's pod log, so it may list the pods in kube-system and read their logs."
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

variable "chart_repositories" {
  description = "Where clusters install their Helm charts from when the charts' own repositories are out of reach. mirrors maps a repository host (ghcr.io, charts.jetstack.io) to a mirror that serves the same charts under the same digests: an oci:// address for a host that serves OCI charts, an http(s):// one for a host that serves a chart repository. ca_bundle (PEM) is the authority of mirrors the usual authorities do not sign."
  type = object({
    mirrors   = optional(map(string), {})
    ca_bundle = optional(string, "")
  })
  default  = {}
  nullable = false

  validation {
    condition     = alltrue([for host, url in var.chart_repositories.mirrors : can(regex("^[A-Za-z0-9.-]+(:[0-9]+)?$", host)) && can(regex("^(oci|https?)://[^/?#[:space:]]+(/[^?#[:space:]]*)?$", url)) && !endswith(url, "/")])
    error_message = "chart_repositories.mirrors maps a host name to an oci://, https:// or http:// address with a host, without a trailing slash."
  }
  validation {
    condition     = var.chart_repositories.ca_bundle == "" || length(var.chart_repositories.mirrors) > 0
    error_message = "chart_repositories.ca_bundle needs mirrors."
  }
}

variable "proxy" {
  description = "The HTTP(S) proxy every download from the management side goes through: url (with credentials if it needs them), no_proxy (comma-separated hosts, domains and networks that bypass it, which has to name the Proxmox VE API host, the nodes' networks and the pod and service ranges) and ca_bundle (PEM, for a proxy that re-signs TLS). Empty strings go direct."
  type = object({
    url       = optional(string, "")
    no_proxy  = optional(string, "")
    ca_bundle = optional(string, "")
  })
  default   = {}
  nullable  = false
  sensitive = true

  validation {
    condition     = nonsensitive(var.proxy.url == "" || can(regex("^https?://[^/?#]+/?$", var.proxy.url)))
    error_message = "proxy.url is an http:// or https:// address with a host and no path."
  }
  validation {
    condition     = nonsensitive(var.proxy.url != "" || (var.proxy.no_proxy == "" && var.proxy.ca_bundle == ""))
    error_message = "proxy.no_proxy and proxy.ca_bundle need proxy.url."
  }
}
