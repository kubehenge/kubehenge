locals {
  nodes = [for i, address in var.node_addresses : {
    name         = format("%s-cp-%d", var.cluster_name, i)
    proxmox_node = var.proxmox_nodes[i % length(var.proxmox_nodes)]
    address      = address
    ip           = split("/", address)[0]
    # Proxmox VE's own prefix and 24 bits from the name, so Talos can name the link before the VM exists.
    mac = upper(join(":", concat(["bc", "24", "11"], regexall("..", substr(md5("${var.cluster_name}-${i}"), 0, 6)))))
  }]
  node_ips     = [for n in local.nodes : n.ip]
  api_endpoint = "https://${var.api_address}:6443"

  kube = ephemeral.talos_cluster_kubeconfig.this.kubernetes_client_configuration

  # Talos drops node addresses that fall inside the pod or service ranges, which leaves a node
  # without an address, so overlaps are refused before anything is created.
  cluster_ranges = concat(var.pod_subnets, var.service_subnets)
  host_ranges = distinct(concat(
    [for a in var.node_addresses : cidrsubnet(a, 0, 0)],
    ["${var.api_address}/32", "${var.gateway}/32"],
  ))
  # First and last address of each range, as numbers.
  range_bounds = {
    for c in distinct(concat(local.cluster_ranges, local.host_ranges)) :
    c => [for h in [0, -1] : sum([for i, octet in split(".", cidrhost(c, h)) : tonumber(octet) * pow(256, 3 - i)])]
  }
  address_overlaps = flatten([
    for i, c in local.cluster_ranges : [
      for o in concat(local.host_ranges, slice(local.cluster_ranges, i + 1, length(local.cluster_ranges))) :
      "${c} overlaps ${o}"
      if local.range_bounds[c][0] <= local.range_bounds[o][1] && local.range_bounds[o][0] <= local.range_bounds[c][1]
    ]
  ])
}

provider "proxmox" {
  endpoint  = var.proxmox_endpoint
  api_token = var.proxmox_api_token
  insecure  = var.proxmox_insecure
}

provider "helm" {
  kubernetes = {
    host                   = local.kube.host
    cluster_ca_certificate = base64decode(local.kube.ca_certificate)
    client_certificate     = base64decode(local.kube.client_certificate)
    client_key             = base64decode(local.kube.client_key)
  }
}
