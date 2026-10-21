locals {
  # The machine configuration schema the cluster was created with. Talos upgrades leave it as it is;
  # changing it regenerates every node's configuration.
  talos_config_contract = "v1.14"

  talos_image = data.talos_image_factory_urls.this.urls.installer

  # Talos and the node's container runtime pull these only with their publishers' keyless
  # signatures: the Image Factory's builds of a release differ in their bytes, so no digest of its
  # installer stays valid, and Talos's Kubernetes upgrades name the components by tag. Sidero signs
  # its images with its release service account or a siderolabs.com account; Kubernetes' image
  # promoter signs everything on registry.k8s.io. A registry mirror has to serve the signatures too.
  image_signatures = [
    { image = "factory.talos.dev/*", keyless = { issuer = "https://accounts.google.com", subject = "image-factory-signing@talos-production.iam.gserviceaccount.com" } },
    { image = "ghcr.io/siderolabs/*", keyless = { issuer = "https://accounts.google.com", subjectRegex = "^(releasemgr-svc@talos-production\\.iam\\.gserviceaccount\\.com|[^@]+@siderolabs\\.com)$" } },
    { image = "registry.k8s.io/*", keyless = { issuer = "https://accounts.google.com", subject = "krel-trust@k8s-releng-prod.iam.gserviceaccount.com" } },
  ]

  patient_lease_args = {
    leader-elect-lease-duration = local.patient_leases.leaderElection.leaseDuration
    leader-elect-renew-deadline = local.patient_leases.leaderElection.renewDeadline
    leader-elect-retry-period   = local.patient_leases.leaderElection.retryPeriod
  }
}

resource "talos_image_factory_schematic" "this" {
  schematic = yamlencode({
    customization = {
      systemExtensions = {
        officialExtensions = ["siderolabs/qemu-guest-agent"]
      }
    }
  })
}

data "talos_image_factory_urls" "this" {
  talos_version     = local.versions.talos
  schematic_id      = talos_image_factory_schematic.this.id
  platform          = "nocloud"
  disk_image_format = "qcow2"
}

resource "talos_machine_secrets" "this" {
  talos_version = local.talos_config_contract
}

data "talos_client_configuration" "this" {
  cluster_name         = var.cluster_name
  client_configuration = talos_machine_secrets.this.client_configuration
  endpoints            = local.node_ips
  nodes                = local.node_ips
}

data "talos_machine_configuration" "node" {
  count = length(local.nodes)

  cluster_name       = var.cluster_name
  cluster_endpoint   = local.api_endpoint
  machine_type       = "controlplane"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  talos_version      = local.talos_config_contract
  kubernetes_version = local.versions.kubernetes

  # Patches apply in order: the first removes generated documents the second replaces.
  config_patches = [
    join("---\n", [
      # A fixed hostname instead of one derived from the machine identity.
      yamlencode({ apiVersion = "v1alpha1", kind = "HostnameConfig", "$patch" = "delete" }),
      # No membership reports to Sidero's public discovery service; nothing here uses it.
      yamlencode({ apiVersion = "v1alpha1", kind = "DiscoveryServiceConfig", name = "default", "$patch" = "delete" }),
    ]),
    join("---\n", concat([
      yamlencode({ apiVersion = "v1alpha1", kind = "HostnameConfig", hostname = local.nodes[count.index].name }),
      yamlencode({
        apiVersion = "v1alpha1"
        kind       = "UnattendedInstallConfig"
        installer  = { image = local.talos_image }
        # The VM's only disk, on the virtio-scsi controller.
        provisioning = { diskSelector = { match = "disk.dev_path == \"/dev/sda\"" } }
      }),
      yamlencode({
        apiVersion     = "v1alpha1"
        kind           = "KubeNetworkConfig"
        podSubnets     = var.pod_subnets
        serviceSubnets = var.service_subnets
      }),
      # The management cluster has no workers: its controllers run on the control plane.
      yamlencode({
        apiVersion = "v1alpha1"
        kind       = "KubeNodeConfig"
        taints     = { "node-role.kubernetes.io/control-plane" = { "$patch" = "delete" } }
      }),
      # The leases of the Cluster API providers (local.patient_leases), for the same API stalls.
      yamlencode({ apiVersion = "v1alpha1", kind = "KubeControllerManagerConfig", extraArgs = local.patient_lease_args }),
      yamlencode({ apiVersion = "v1alpha1", kind = "KubeSchedulerConfig", extraArgs = local.patient_lease_args }),
      yamlencode({
        apiVersion = "v1alpha1"
        kind       = "LinkAliasConfig"
        name       = "lan"
        selector   = { match = "mac(link.permanent_addr) == \"${lower(local.nodes[count.index].mac)}\"" }
      }),
      yamlencode({
        apiVersion = "v1alpha1"
        kind       = "Layer2VIPConfig"
        name       = var.api_address
        link       = "lan"
      }),
      yamlencode({ apiVersion = "v1alpha1", kind = "ImageVerificationConfig", rules = local.image_signatures }),
      ], [
      for registry, endpoints in var.registry_mirrors : yamlencode({
        apiVersion = "v1alpha1"
        kind       = "RegistryMirrorConfig"
        name       = registry
        endpoints  = [for e in endpoints : { url = e }]
      })
    ])),
  ]
}

ephemeral "talos_cluster_kubeconfig" "this" {
  cluster_name    = var.cluster_name
  endpoint        = local.api_endpoint
  machine_secrets = talos_machine_secrets.this.machine_secrets
}

# One block per possible node, each waiting for the one before, so an upgrade reboots one
# control-plane node at a time whatever -parallelism the apply runs with.
module "talos_node_0" {
  source = "./modules/talos-node"

  node                  = local.nodes[0].ip
  client_configuration  = talos_machine_secrets.this.client_configuration
  machine_configuration = data.talos_machine_configuration.node[0].machine_configuration
  image                 = local.talos_image
  # With one node there is nowhere to drain to.
  drain_on_upgrade = length(local.nodes) > 1
  kubeconfig       = ephemeral.talos_cluster_kubeconfig.this.kubeconfig_raw

  depends_on = [proxmox_virtual_environment_vm.node]
}

module "talos_node_1" {
  source = "./modules/talos-node"
  count  = length(local.nodes) > 1 ? 1 : 0

  node                  = local.nodes[1].ip
  client_configuration  = talos_machine_secrets.this.client_configuration
  machine_configuration = data.talos_machine_configuration.node[1].machine_configuration
  image                 = local.talos_image
  drain_on_upgrade      = true
  kubeconfig            = ephemeral.talos_cluster_kubeconfig.this.kubeconfig_raw

  depends_on = [module.talos_node_0]
}

module "talos_node_2" {
  source = "./modules/talos-node"
  count  = length(local.nodes) > 2 ? 1 : 0

  node                  = local.nodes[2].ip
  client_configuration  = talos_machine_secrets.this.client_configuration
  machine_configuration = data.talos_machine_configuration.node[2].machine_configuration
  image                 = local.talos_image
  drain_on_upgrade      = true
  kubeconfig            = ephemeral.talos_cluster_kubeconfig.this.kubeconfig_raw

  depends_on = [module.talos_node_1]
}

module "talos_node_3" {
  source = "./modules/talos-node"
  count  = length(local.nodes) > 3 ? 1 : 0

  node                  = local.nodes[3].ip
  client_configuration  = talos_machine_secrets.this.client_configuration
  machine_configuration = data.talos_machine_configuration.node[3].machine_configuration
  image                 = local.talos_image
  drain_on_upgrade      = true
  kubeconfig            = ephemeral.talos_cluster_kubeconfig.this.kubeconfig_raw

  depends_on = [module.talos_node_2]
}

module "talos_node_4" {
  source = "./modules/talos-node"
  count  = length(local.nodes) > 4 ? 1 : 0

  node                  = local.nodes[4].ip
  client_configuration  = talos_machine_secrets.this.client_configuration
  machine_configuration = data.talos_machine_configuration.node[4].machine_configuration
  image                 = local.talos_image
  drain_on_upgrade      = true
  kubeconfig            = ephemeral.talos_cluster_kubeconfig.this.kubeconfig_raw

  depends_on = [module.talos_node_3]
}

resource "talos_cluster" "this" {
  node                 = local.nodes[0].ip
  control_plane_nodes  = local.node_ips
  client_configuration = talos_machine_secrets.this.client_configuration
  kubernetes_version   = local.versions.kubernetes

  timeouts = {
    create = "20m"
    update = "40m"
  }

  depends_on = [module.talos_node_0, module.talos_node_1, module.talos_node_2, module.talos_node_3, module.talos_node_4]
}

data "talos_cluster_health" "this" {
  client_configuration = talos_machine_secrets.this.client_configuration
  endpoints            = local.node_ips
  control_plane_nodes  = local.node_ips

  timeouts = {
    read = "15m"
  }

  # The nodes too, not only talos_cluster: a data source is read at plan time unless something it
  # depends on directly changes, and a Talos upgrade changes the nodes alone. Read then, it would
  # let everything after it run while the upgraded nodes are still coming back.
  depends_on = [talos_cluster.this, module.talos_node_0, module.talos_node_1, module.talos_node_2, module.talos_node_3, module.talos_node_4]
}

resource "talos_cluster_kubeconfig" "this" {
  client_configuration = talos_machine_secrets.this.client_configuration
  node                 = local.nodes[0].ip

  depends_on = [talos_cluster.this]
}
