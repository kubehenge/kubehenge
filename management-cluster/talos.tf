locals {
  # The machine configuration schema the cluster was created with. Talos upgrades leave it as it is;
  # changing it regenerates every node's configuration.
  talos_config_contract = "v1.14"

  talos_image = data.talos_image_factory_urls.this.urls.installer

  # Talos and the node's container runtime pull these only with their publishers' keyless
  # signatures: the Image Factory's builds of a release differ in their bytes, so no digest of its
  # installer stays valid, and Talos's Kubernetes upgrades name the components by tag. Sidero signs
  # its images with its release service account or a siderolabs.com account. Kubernetes' image
  # promoter signs everything on registry.k8s.io with two accounts that only its production jobs
  # use: krel-trust for the promotion, promoter-summaries for its verification summary. Talos reads
  # every bundle and pulls once, so accepting both lets one failed bundle fetch not fail the pull.
  # A registry mirror has to serve the signatures too.
  image_signatures = [
    { image = "factory.talos.dev/*", keyless = { issuer = "https://accounts.google.com", subject = "image-factory-signing@talos-production.iam.gserviceaccount.com" } },
    { image = "ghcr.io/siderolabs/*", keyless = { issuer = "https://accounts.google.com", subjectRegex = "^(releasemgr-svc@talos-production\\.iam\\.gserviceaccount\\.com|[^@]+@siderolabs\\.com)$" } },
    { image = "registry.k8s.io/*", keyless = { issuer = "https://accounts.google.com", subjectRegex = "^(krel-trust|promoter-summaries)@k8s-releng-prod\\.iam\\.gserviceaccount\\.com$" } },
  ]
  # The Talos providers' controllers, which only the management cluster runs, come from Kubehenge's
  # forks of them and are pulled only as each fork's release workflow signed them for a version tag,
  # keyless through GitHub's OIDC issuer.
  provider_image_signatures = [for repository in ["cluster-api-bootstrap-provider-talos", "cluster-api-control-plane-provider-talos"] : {
    image = "ghcr.io/kubehenge/${repository}"
    keyless = {
      issuer       = "https://token.actions.githubusercontent.com"
      subjectRegex = "^https://github\\.com/kubehenge/${repository}/\\.github/workflows/release\\.yaml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.]+)?$"
    }
  }]

  patient_lease_args = {
    leader-elect-lease-duration = local.patient_leases.leaderElection.leaseDuration
    leader-elect-renew-deadline = local.patient_leases.leaderElection.renewDeadline
    leader-elect-retry-period   = local.patient_leases.leaderElection.retryPeriod
  }

  # In milliseconds. The election timeout stays ten heartbeats, as etcd's tuning guide asks.
  etcd_timing_args = {
    heartbeat-interval = "500"
    election-timeout   = "5000"
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
      # Replaces Talos's default audit policy, so the policy below is the only one.
      yamlencode({ apiVersion = "v1alpha1", kind = "KubeAuditPolicyConfig", "$patch" = "delete" }),
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
        podSubnets     = local.pod_subnets
        serviceSubnets = local.service_subnets
      }),
      # Flannel enforces no NetworkPolicy by itself; Talos runs kube-network-policies beside it,
      # which does, with kube-proxy left as it is.
      yamlencode({ apiVersion = "v1alpha1", kind = "KubeFlannelCNIConfig", kubeNetworkPoliciesEnabled = true }),
      # The management cluster has no workers: its controllers run on the control plane. With IPv6
      # the kubelet registers one address per family, primary first, picked from the nodes'
      # networks so the VIP is never taken. Talos refuses that setting in .machine.kubelet beside
      # this document. The zone label names the Proxmox VE node the VM runs on, which a migration
      # changes, so Kubehenge alone sets it. The appliance does after each apply, the controller
      # before each wake of the backup store and on a node still without one. The CSI node plugin
      # restarts until it is set. Set here, a kubelet start would put back the node the VM was made on. Talos refuses .machine.nodeLabels beside this
      # document too.
      yamlencode(merge({
        apiVersion = "v1alpha1"
        kind       = "KubeNodeConfig"
        taints     = { "node-role.kubernetes.io/control-plane" = { "$patch" = "delete" } }
        labels = {
          "topology.kubernetes.io/region" = local.csi_region
        }
        }, local.has_ipv6 ? {
        nodeIP = { validSubnets = flatten(local.node_networks) }
      } : {})),
      # Image pulls in parallel, so that one stalled on a slow registry holds no other image of the
      # node back. Three leave two free and stay within the kubelet's registry rate of 5 a second.
      yamlencode({ apiVersion = "v1alpha1", kind = "KubeletConfig", config = { serializeImagePulls = false, maxParallelImagePulls = 3 } }),
      # The leases of the Cluster API providers (local.patient_leases), for the same API stalls.
      yamlencode({ apiVersion = "v1alpha1", kind = "KubeControllerManagerConfig", extraArgs = local.patient_lease_args }),
      yamlencode({ apiVersion = "v1alpha1", kind = "KubeSchedulerConfig", extraArgs = local.patient_lease_args }),
      # Events kept a day rather than an hour: a failed run stays listed that long, and its events
      # are its log. With api_audit_log, the audit log goes to the API server's output instead of
      # its file, where the interface reads the changes people made as a pod log.
      yamlencode({
        apiVersion = "v1alpha1"
        kind       = "KubeAPIServerConfig"
        extraArgs  = merge({ event-ttl = "24h" }, var.api_audit_log ? { audit-log-path = "-" } : {})
      }),
      # The workload clusters' audit policy, from its one file. It leaves out lease renewals and
      # health checks, which would fill the log's 10 MiB within minutes.
      yamlencode({
        apiVersion    = "v1alpha1"
        kind          = "KubeAuditPolicyConfig"
        configuration = yamldecode(file("${path.module}/../charts/kubehenge/files/audit-policy.yaml"))
      }),
      yamlencode({
        apiVersion = "v1alpha1"
        kind       = "LinkAliasConfig"
        name       = "lan"
        # The node's one NIC, by its driver, as Talos's own QEMU provisioner selects it. A fixed
        # alias must match exactly one link. The VM's mac_addresses come from the guest agent and
        # start with the loopback's, so they cannot name it.
        selector = { match = "link.driver == \"virtio_net\"" }
      }),
      yamlencode({
        apiVersion = "v1alpha1"
        kind       = "Layer2VIPConfig"
        name       = var.api_address
        link       = "lan"
      }),
      yamlencode({ apiVersion = "v1alpha1", kind = "ImageVerificationConfig", rules = concat(local.image_signatures, local.provider_image_signatures) }),
      ], [
      for registry, endpoints in var.registry_mirrors : yamlencode({
        apiVersion = "v1alpha1"
        kind       = "RegistryMirrorConfig"
        name       = registry
        endpoints  = [for e in endpoints : { url = e }]
      })
      ],
      # Without servers, Talos keeps its default public ones, which a site may block on UDP 123.
      length(var.time_servers) == 0 ? [] : [yamlencode({
        apiVersion = "v1alpha1"
        kind       = "TimeSyncConfig"
        ntp        = { servers = var.time_servers }
      })],
      local.proxy_enabled ? concat(
        # The proxy for Talos's own services, containerd's image pulls among them.
        [yamlencode({ machine = { env = local.proxy_talos_env } })],
        local.proxy_has_ca ? [yamlencode({ apiVersion = "v1alpha1", kind = "TrustedRootsConfig", name = "kubehenge-proxy", certificates = var.proxy.ca_bundle })] : [],
      ) : [],
      # Image pulls and every other TLS connection of the nodes trust the site's CA, which TLS
      # inspection without a proxy needs as much as a proxy does.
      var.site_ca == "" ? [] : [yamlencode({ apiVersion = "v1alpha1", kind = "TrustedRootsConfig", name = "kubehenge-site", certificates = var.site_ca })],
      # The same MTU as the Proxmox VE interface, on the link by its alias, so it holds whatever
      # the guest driver negotiates.
      var.mtu == null ? [] : [yamlencode({ apiVersion = "v1alpha1", kind = "LinkConfig", name = "lan", mtu = var.mtu })],
      # The nameservers stay the platform's: only search domains are set.
      length(var.search_domains) == 0 ? [] : [yamlencode({
        apiVersion    = "v1alpha1"
        kind          = "ResolverConfig"
        searchDomains = { domains = var.search_domains }
      })],
      # etcd's own timings fit a LAN with SSDs. It fails a request after 5 s plus two election
      # timeouts, 7 s by default, and storage under load (HDD-backed LVM, NFS, a busy Ceph pool)
      # holds a sync longer than that. With these it waits 15 s. A dead leader is replaced after 5 s
      # instead of 1 s. With IPv6, etcd advertises a node address of the primary family, never the
      # VIP or an address of the other family.
      [yamlencode({
        cluster = { etcd = merge(
          { extraArgs = local.etcd_timing_args },
          local.has_ipv6 ? { advertisedSubnets = local.node_networks[0] } : {},
        ) }
      })],
    )),
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
  vm_id                 = proxmox_virtual_environment_vm.node[0].id
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
  vm_id                 = proxmox_virtual_environment_vm.node[1].id
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
  vm_id                 = proxmox_virtual_environment_vm.node[2].id
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
  vm_id                 = proxmox_virtual_environment_vm.node[3].id
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
  vm_id                 = proxmox_virtual_environment_vm.node[4].id
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
