locals {
  has_ipv4     = contains(var.ip_families, "IPv4")
  has_ipv6     = contains(var.ip_families, "IPv6")
  primary_ipv6 = var.ip_families[0] == "IPv6"

  # The primary family's addresses give the node count and the address Talos is reached on.
  primary_addresses = local.primary_ipv6 ? var.node_addresses_ipv6 : var.node_addresses
  nodes = [for i, address in local.primary_addresses : {
    name         = format("%s-cp-%d", var.cluster_name, i)
    proxmox_node = var.proxmox_nodes[i % length(var.proxmox_nodes)]
    address      = address
    address_ipv4 = local.has_ipv4 ? try(var.node_addresses[i], null) : null
    address_ipv6 = local.has_ipv6 ? try(var.node_addresses_ipv6[i], null) : null
    ip           = split("/", address)[0]
  }]
  node_ips = [for n in local.nodes : n.ip]
  # An IPv6 literal is bracketed in a URL.
  api_endpoint = "https://${strcontains(var.api_address, ":") ? "[${var.api_address}]" : var.api_address}:6443"

  # The nodes' network of each family, in ip_families order.
  node_networks = [for f in var.ip_families : distinct([for a in(f == "IPv6" ? var.node_addresses_ipv6 : var.node_addresses) : cidrsubnet(a, 0, 0)])]

  kube = ephemeral.talos_cluster_kubeconfig.this.kubernetes_client_configuration

  default_pod_subnets     = { IPv4 = "10.244.0.0/16", IPv6 = "fd5e:4b1c:9a20::/56" }
  default_service_subnets = { IPv4 = "10.96.0.0/12", IPv6 = "fd5e:4b1c:9a21::/112" }
  pod_subnets             = var.pod_subnets == null ? [for f in var.ip_families : local.default_pod_subnets[f]] : var.pod_subnets
  service_subnets         = var.service_subnets == null ? [for f in var.ip_families : local.default_service_subnets[f]] : var.service_subnets

  # Talos drops node addresses that fall inside the pod or service ranges, which leaves a node
  # without an address, so overlaps are refused before anything is created.
  cluster_ranges = concat(local.pod_subnets, local.service_subnets)
  host_ranges = distinct(concat(
    [for a in concat(var.node_addresses, var.node_addresses_ipv6) : cidrsubnet(a, 0, 0)],
    [for h in compact([var.api_address, var.gateway, var.gateway_ipv6]) : "${h}/${strcontains(h, ":") ? 128 : 32}"],
  ))
  range_info = {
    for c in distinct(concat(local.cluster_ranges, local.host_ranges)) :
    c => { v6 = strcontains(c, ":"), ip = split("/", c)[0], bits = tonumber(split("/", c)[1]) }
  }
  # OpenTofu has no 128-bit arithmetic. Two networks of one family overlap when both, cut to the
  # shorter prefix length, are the same network.
  range_pairs = flatten([
    for i, c in local.cluster_ranges : [
      for o in concat(local.host_ranges, slice(local.cluster_ranges, i + 1, length(local.cluster_ranges))) :
      { c = c, o = o, bits = min(local.range_info[c].bits, local.range_info[o].bits) }
    ]
  ])
  address_overlaps = [
    for p in local.range_pairs : "${p.c} overlaps ${p.o}"
    if local.range_info[p.c].v6 == local.range_info[p.o].v6 &&
    cidrsubnet("${local.range_info[p.c].ip}/${p.bits}", 0, 0) == cidrsubnet("${local.range_info[p.o].ip}/${p.bits}", 0, 0)
  ]
}

provider "proxmox" {
  endpoint  = var.proxmox_endpoint
  api_token = var.proxmox_api_token
  insecure  = var.proxmox_insecure

  # Proxmox VE reserves no ID it offers as the next free one, so a template build, a cluster's
  # machine or anything else created at the same moment can be offered the same ID. When its
  # create is refused for that, the provider retries it and takes the second refusal for a create
  # of its own that went through: it configures and starts the other VM and keeps it in its state,
  # to be destroyed with the node. Random IDs from a range of their own keep the nodes away from
  # the IDs every other creator is offered. The range is the caller's, to stay inside the band an
  # administrator reserved with the datacenter's next-id option. The provider draws from start up
  # to, not including, end. It is provider configuration, so a VM resource's plan never sees it.
  random_vm_ids      = true
  random_vm_id_start = var.vm_id_range.start
  random_vm_id_end   = var.vm_id_range.end
}

provider "helm" {
  kubernetes = {
    host                   = local.kube.host
    cluster_ca_certificate = base64decode(local.kube.ca_certificate)
    client_certificate     = base64decode(local.kube.client_certificate)
    client_key             = base64decode(local.kube.client_key)
  }
}
