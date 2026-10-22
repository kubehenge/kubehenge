locals {
  kubehenge_namespace = "kubehenge-system"
  capmox_namespace    = "proxmox-infrastructure-system"

  # The Proxmox VE provider's token secret; the token ID is the part before it.
  cluster_api_token_secret = reverse(split("=", proxmox_user_token.cluster_api.value))[0]

  controller_secret = { name = "kubehenge-proxmox", namespace = local.kubehenge_namespace }
  builder_secret    = { name = "kubehenge-template-builder", namespace = local.kubehenge_namespace }

  # The Helm provider upgrades a release only when its chart's version or its values change. Each
  # release gets the digest of the files Helm packages from its chart, so a changed chart is
  # upgraded whatever its version says.
  chart_digests = { for name, dir in {
    kubehenge             = "${path.module}/../charts/kubehenge"
    cluster_api_providers = "${path.module}/charts/cluster-api-providers"
    } : name => sha256(join("\n", [
      for f in sort(fileset(dir, "{Chart.yaml,values.yaml,values.schema.json,templates/**,files/**}")) : "${f} ${filesha256("${dir}/${f}")}"
  ])) }

  # The providers' release runs it too, for its hooks, before the controller's chart is installed.
  # A release's chart names the image it was released with, by digest; a checkout's names none.
  controller_image = var.controller_image != null ? var.controller_image : yamldecode(file("${path.module}/../charts/kubehenge/values.yaml")).image

  cluster_api_variables = {
    name      = "cluster-api-variables"
    namespace = local.kubehenge_namespace
    # Runtime SDK: Kubehenge's controller holds new clusters until their Proxmox VE objects exist.
    data = { CLUSTER_TOPOLOGY = "true", EXP_RUNTIME_SDK = "true" }
  }
  capmox_variables = {
    name      = "capmox-variables"
    namespace = local.capmox_namespace
    data = {
      PROXMOX_URL     = var.proxmox_endpoint
      PROXMOX_TOKEN   = proxmox_user_token.cluster_api.id
      ClusterTopology = "true"
    }
  }

  # A provider's variables change its pods without changing the provider object, so the release's
  # wait for the providers would pass against pods still running the old ones. Each provider that
  # takes variables carries a hash of them, which turns a change into a new generation to wait for.
  # A hash of the token secret reveals nothing about it.
  config_hash = {
    (local.cluster_api_variables.name) = sha256(jsonencode(local.cluster_api_variables.data))
    (local.capmox_variables.name)      = nonsensitive(sha256(jsonencode([local.capmox_variables.data, local.cluster_api_token_secret, var.proxmox_ca_certificate, var.proxmox_insecure])))
  }
  config_hash_patch = { for name, hash in local.config_hash : name => {
    target = { kind = "Deployment" }
    patch  = yamlencode({ spec = { template = { metadata = { annotations = { "kubehenge.dev/config-hash" = hash } } } } })
  } }
  cluster_api_config = { name = local.cluster_api_variables.name, namespace = local.cluster_api_variables.namespace }
  capmox_config      = { name = local.capmox_variables.name, namespace = local.capmox_variables.namespace }

  # Upstream defaults to skipping certificate verification; this turns it on against the given CA,
  # unless proxmox_insecure says otherwise.
  capmox_verify_tls = [
    { op = "add", path = "/spec/template/spec/containers/0/args/-", value = "--proxmox-insecure=${var.proxmox_insecure}" },
    { op = "add", path = "/spec/template/spec/containers/0/args/-", value = "--proxmox-root-cert-file=/etc/proxmox-ca/ca.crt" },
    { op = "add", path = "/spec/template/spec/containers/0/volumeMounts/-", value = { name = "proxmox-ca", mountPath = "/etc/proxmox-ca", readOnly = true } },
    { op = "add", path = "/spec/template/spec/volumes/-", value = { name = "proxmox-ca", secret = { secretName = "proxmox-ca" } } },
  ]

  # A distribution's providers run only while a cluster uses it: Kubehenge's controller scales their
  # Deployments between none and one. The operator applies the manifests again whenever it looks at
  # a provider, so the replica count comes out of them, or each look would start them again.
  replicas_left_to_scaling = { target = { kind = "Deployment" }, patch = yamlencode({ spec = { replicas = null } }) }

  # Cluster API's leases (15 s, renewed within 10 s) are shorter than the API server can stall
  # while Proxmox VE clones a disk on the management cluster's own storage; a controller that
  # misses its renewal exits, and its webhooks refuse requests until it is back. These are the
  # timings of Kubehenge's own controller, also given to the operator, kube-controller-manager and
  # kube-scheduler. The k3s and in-cluster IPAM providers take no such flags. The lease names stay
  # empty, or the operator would pass a flag the providers lack.
  patient_leases = { leaderElection = {
    leaderElect       = true
    leaseDuration     = "60s"
    renewDeadline     = "40s"
    retryPeriod       = "10s"
    resourceLock      = ""
    resourceName      = ""
    resourceNamespace = ""
  } }

  # The providers Kubehenge's ClusterClasses use, and no others; each gets only the variables it
  # uses. Talos's join with a Talos ClusterClass.
  provider_specs = [
    { kind = "CoreProvider", name = "cluster-api", namespace = "capi-system", spec = {
      version      = local.versions.cluster_api
      configSecret = local.cluster_api_config
      patches      = [local.config_hash_patch[local.cluster_api_config.name]]
      manager      = local.patient_leases
    } },
    { kind = "BootstrapProvider", name = "kubeadm", namespace = "kubeadm-bootstrap-system", spec = {
      version = local.versions.cluster_api
      patches = [local.replicas_left_to_scaling]
      manager = local.patient_leases
    } },
    { kind = "BootstrapProvider", name = "rke2", namespace = "rke2-bootstrap-system", spec = {
      version = local.versions.rke2
      patches = [local.replicas_left_to_scaling]
      manager = local.patient_leases
    } },
    { kind = "BootstrapProvider", name = "k3s", namespace = "k3s-bootstrap-system", spec = {
      version = local.versions.k3s
      patches = [local.replicas_left_to_scaling]
    } },
    { kind = "ControlPlaneProvider", name = "kubeadm", namespace = "kubeadm-control-plane-system", spec = {
      version      = local.versions.cluster_api
      configSecret = local.cluster_api_config
      patches      = [local.config_hash_patch[local.cluster_api_config.name], local.replicas_left_to_scaling]
      manager      = local.patient_leases
    } },
    { kind = "ControlPlaneProvider", name = "rke2", namespace = "rke2-control-plane-system", spec = {
      version = local.versions.rke2
      patches = [local.replicas_left_to_scaling]
      manager = local.patient_leases
    } },
    { kind = "ControlPlaneProvider", name = "k3s", namespace = "k3s-control-plane-system", spec = {
      version = local.versions.k3s
      patches = [local.replicas_left_to_scaling]
    } },
    { kind = "InfrastructureProvider", name = "proxmox", namespace = local.capmox_namespace, spec = {
      version      = local.versions.capmox
      configSecret = local.capmox_config
      patches = [
        {
          target = { kind = "Deployment", name = "capmox-controller-manager" }
          patch  = yamlencode(local.capmox_verify_tls)
        },
        local.config_hash_patch[local.capmox_config.name],
      ]
      manager = local.patient_leases
    } },
    { kind = "IPAMProvider", name = "in-cluster", namespace = "in-cluster-ipam-system", spec = { version = local.versions.ipam_in_cluster } },
    { kind = "AddonProvider", name = "helm", namespace = "helm-addon-system", spec = {
      version = local.versions.helm_addon
      manager = local.patient_leases
    } },
  ]
  # Each of Kubehenge's ClusterClasses and the namespaces of the providers only its clusters use.
  class_provider_namespaces = { for d in ["kubeadm", "rke2", "k3s"] : "kubehenge-${d}" => sort([
    for p in local.provider_specs : p.namespace if p.name == d
  ]) }

  # Requests for the providers whose manifests carry none, near what each holds: without one a
  # controller is BestEffort, the first a memory-short node kills, and Cluster API's core is the
  # one every cluster depends on. No limits: a controller that grows while it syncs must not be
  # killed for it.
  memory_requests = {
    "CoreProvider/cluster-api"     = "128Mi"
    "BootstrapProvider/kubeadm"    = "64Mi"
    "BootstrapProvider/rke2"       = "64Mi"
    "BootstrapProvider/k3s"        = "64Mi"
    "ControlPlaneProvider/kubeadm" = "64Mi"
    "ControlPlaneProvider/k3s"     = "64Mi"
    "AddonProvider/helm"           = "64Mi"
  }

  # The component of the tested set each provider comes with.
  provider_components = {
    "CoreProvider/cluster-api"       = "cluster_api"
    "BootstrapProvider/kubeadm"      = "cluster_api"
    "BootstrapProvider/rke2"         = "rke2"
    "BootstrapProvider/k3s"          = "k3s"
    "ControlPlaneProvider/kubeadm"   = "cluster_api"
    "ControlPlaneProvider/rke2"      = "rke2"
    "ControlPlaneProvider/k3s"       = "k3s"
    "InfrastructureProvider/proxmox" = "capmox"
    "IPAMProvider/in-cluster"        = "ipam_in_cluster"
    "AddonProvider/helm"             = "helm_addon"
  }
  components_files = {
    CoreProvider           = "core-components.yaml"
    BootstrapProvider      = "bootstrap-components.yaml"
    ControlPlaneProvider   = "control-plane-components.yaml"
    InfrastructureProvider = "infrastructure-components.yaml"
    IPAMProvider           = "ipam-components.yaml"
    AddonProvider          = "addon-components.yaml"
  }
  manifest_digests = merge(local.tested_manifests, var.provider_manifests)
  # Each provider's files by their address under https://github.com/, as the digests are keyed.
  manifest_paths = { for p in local.provider_specs : "${p.kind}/${p.name}" => {
    for role, file in { metadata = "metadata.yaml", components = local.components_files[p.kind] } :
    role => "${local.provider_repositories[local.provider_components["${p.kind}/${p.name}"]]}/releases/download/${p.spec.version}/${file}"
  } }
  missing_manifests = sort(distinct([for path in flatten([for files in values(local.manifest_paths) : values(files)]) : path if !contains(keys(local.manifest_digests), path)]))

  # A provider's image by digest: one given, Kubehenge's build while it applies, or the tested one at
  # the tested version; null when none of these is.
  provider_images = { for key, component in local.provider_components : key => (
    contains(keys(var.provider_images), key) ? var.provider_images[key] :
    key == "InfrastructureProvider/proxmox" && local.versions.capmox == local.capmox_build.release ? local.capmox_build.image :
    key == "AddonProvider/helm" && local.versions.helm_addon == local.helm_addon_build.release ? local.helm_addon_build.image :
    local.versions[component] == local.tested_versions[component] ? local.tested_provider_images[key] : null
  ) }
  missing_images = sort([for key, image in local.provider_images : key if image == null])

  # Every Cluster API provider names its controller container "manager".
  provider_containers = { for p in local.provider_specs : "${p.kind}/${p.name}" => merge(
    { name = "manager", imageUrl = local.provider_images["${p.kind}/${p.name}"] },
    contains(keys(local.memory_requests), "${p.kind}/${p.name}") ?
    { resources = { requests = { cpu = "10m", memory = local.memory_requests["${p.kind}/${p.name}"] } } } : {},
  ) }
  # The operator installs each provider from the ConfigMap the providers chart's hook stores its
  # checked files in, not from the release.
  providers = [for p in local.provider_specs : merge(p, {
    spec = merge(p.spec, {
      fetchConfig = { selector = { matchLabels = {
        "kubehenge.dev/provider-manifests"  = lower("${p.kind}-${p.name}")
        "provider.cluster.x-k8s.io/version" = p.spec.version
      } } }
      deployment = { containers = [local.provider_containers["${p.kind}/${p.name}"]] }
    })
    # A file without a digest stops the release in its precondition before this is used.
    manifests = { for role, path in local.manifest_paths["${p.kind}/${p.name}"] :
    role => "https://github.com/${path}@sha256:${lookup(local.manifest_digests, path, "none")}" }
  })]
}

resource "helm_release" "cert_manager" {
  name             = "cert-manager"
  namespace        = "cert-manager"
  create_namespace = true
  # Pulled by its digest, which Helm checks the version's tag against.
  repository = "oci://quay.io/jetstack/charts"
  chart      = "cert-manager@sha256:${split("@sha256:", local.cert_manager.chart)[1]}"
  version    = split("@sha256:", local.cert_manager.chart)[0]
  wait       = true

  # The chart sets no requests, which leaves cert-manager BestEffort; see memory_requests. Each image
  # is named by its tag and digest, which the chart puts after the repository.
  values = [yamlencode({
    crds            = { enabled = true }
    image           = { tag = local.cert_manager.images.controller }
    resources       = { requests = { cpu = "10m", memory = "64Mi" } }
    cainjector      = { image = { tag = local.cert_manager.images.cainjector }, resources = { requests = { cpu = "10m", memory = "64Mi" } } }
    webhook         = { image = { tag = local.cert_manager.images.webhook }, resources = { requests = { cpu = "10m", memory = "32Mi" } } }
    startupapicheck = { image = { tag = local.cert_manager.images.startupapicheck } }
    acmesolver      = { image = { tag = local.cert_manager.images.acmesolver } }
  })]

  depends_on = [data.talos_cluster_health.this]
}

# The operator alone; the providers are a release of their own so that a destroy removes them first.
resource "helm_release" "cluster_api_operator" {
  name             = "cluster-api-operator"
  namespace        = "capi-operator-system"
  create_namespace = true
  repository       = "https://kubernetes-sigs.github.io/cluster-api-operator"
  chart            = "cluster-api-operator"
  version          = local.capi_operator.chart
  wait             = true

  values = [yamlencode({
    image             = { manager = { tag = local.capi_operator.image } }
    watchConfigSecret = true
    # The chart starts a pprof endpoint by default; nothing here profiles the operator.
    profilerAddress = ""
    leaderElection = {
      enabled       = true
      leaseDuration = local.patient_leases.leaderElection.leaseDuration
      renewDeadline = local.patient_leases.leaderElection.renewDeadline
      retryPeriod   = local.patient_leases.leaderElection.retryPeriod
    }
  })]

  depends_on = [helm_release.cert_manager]
}

resource "helm_release" "cluster_api_providers" {
  name = "cluster-api-providers"
  # Helm records the release before creating the chart's namespaces, so it lives in the operator's.
  namespace = helm_release.cluster_api_operator.namespace
  chart     = "${path.module}/charts/cluster-api-providers"
  wait      = true
  timeout   = 900

  values = [yamlencode({
    chartDigest = local.chart_digests.cluster_api_providers
    namespaces  = distinct(concat([local.kubehenge_namespace], [for p in local.providers : p.namespace]))
    providers   = local.providers
    image       = local.controller_image
    secrets = merge(
      {
        (local.cluster_api_variables.name) = { namespace = local.cluster_api_variables.namespace, data = local.cluster_api_variables.data }
        (local.capmox_variables.name)      = { namespace = local.capmox_variables.namespace, data = local.capmox_variables.data }
        (local.controller_secret.name) = {
          namespace = local.controller_secret.namespace
          data      = { "ca.crt" = var.proxmox_ca_certificate }
        }
        proxmox-ca = {
          namespace = local.capmox_namespace
          data      = { "ca.crt" = var.proxmox_ca_certificate }
        }
      },
      local.template_builder.enabled ? { (local.builder_secret.name) = { namespace = local.builder_secret.namespace, data = {} } } : {},
    )
  })]

  set_sensitive = concat([
    {
      name  = "secrets.${local.capmox_variables.name}.data.PROXMOX_SECRET"
      value = local.cluster_api_token_secret
      type  = "string"
    },
    {
      name  = "secrets.${local.controller_secret.name}.data.token"
      value = proxmox_user_token.provisioner.value
      type  = "string"
    },
    ], [for t in proxmox_user_token.ha : {
      name  = "secrets.${local.controller_secret.name}.data.ha-token"
      value = t.value
      type  = "string"
      }], [for t in proxmox_user_token.template_builder : {
      name  = "secrets.${local.builder_secret.name}.data.token"
      value = t.value
      type  = "string"
      }], [for t in proxmox_user_token.etcd_restore : {
      name  = "secrets.${local.controller_secret.name}.data.restore-token"
      value = t.value
      type  = "string"
  }])

  lifecycle {
    precondition {
      condition     = local.controller_image != ""
      error_message = "no controller image: a release's chart names its own, and a checkout's needs controller_image."
    }
    precondition {
      condition     = length(local.missing_manifests) == 0
      error_message = "no digest for ${join(", ", local.missing_manifests)}: a version held back needs its files' SHA-256 in provider_manifests."
    }
    precondition {
      condition     = length(local.missing_images) == 0
      error_message = "no image for ${join(", ", local.missing_images)}: a version held back needs its image by digest in provider_images."
    }
  }

  depends_on = [helm_release.cluster_api_operator]
}

resource "helm_release" "kubehenge" {
  name      = "kubehenge"
  namespace = local.kubehenge_namespace
  chart     = "${path.module}/../charts/kubehenge"
  wait      = true

  values = [yamlencode(merge(
    {
      chartDigest        = local.chart_digests.kubehenge
      workloadPool       = proxmox_virtual_environment_pool.workload.pool_id
      providerNamespaces = local.class_provider_namespaces
      managementEndpoint = local.api_endpoint
      interface = {
        extraNamespaces = var.extra_cluster_namespaces
        offerHARules    = var.offer_ha_rules
      }
      clusters = {
        storages = var.workload_datastores
        bridges  = var.workload_bridges
      }
      proxmox = {
        url          = var.proxmox_endpoint
        insecure     = var.proxmox_insecure
        secret       = local.controller_secret.name
        clusterGroup = proxmox_virtual_environment_group.clusters.group_id
        clusterRole  = proxmox_virtual_environment_role.cluster_vms.role_id
        ha           = var.proxmox_ha
        etcdRestore  = var.etcd_restore
        datastoreAllocate = {
          role               = var.cluster_datastore_allocate ? proxmox_virtual_environment_role.cluster_datastore[0].role_id : ""
          managementStorages = local.management_storages
        }
        sysAuditGroup = var.cluster_sys_audit ? proxmox_virtual_environment_group.clusters_audit[0].group_id : ""
        # Written by the appliance, never by this module, so that the password stays out of the
        # state.
        volumeSnapshots = { rootSecret = var.cluster_volume_snapshots ? "kubehenge-root-pam" : "" }
      }
      templateRefresh = {
        enabled       = local.template_builder.enabled
        weekday       = var.template_refresh.weekday
        time          = var.template_refresh.time
        timeZone      = var.template_refresh.time_zone
        soakHours     = var.template_refresh.soak_hours
        secret        = local.builder_secret.name
        node          = local.template_builder.node
        storage       = local.template_builder.storage
        importStorage = local.template_builder.import
        isoStorage    = local.template_builder.iso
        pool          = proxmox_virtual_environment_pool.templates.pool_id
        bridge        = local.template_builder.bridge
        vlan          = var.template_refresh.vlan
        address       = var.template_refresh.address == null ? "" : var.template_refresh.address
        gateway       = var.template_refresh.gateway == null ? "" : var.template_refresh.gateway
        dns           = var.template_refresh.dns
        creatorBuilds = var.template_refresh.creator_builds
      }
      clusterOIDC = [for p in var.cluster_oidc : merge(
        {
          issuerURL      = p.issuer_url
          clientID       = p.client_id
          usernameClaim  = p.username_claim
          usernamePrefix = p.username_prefix
          groupsClaim    = p.groups_claim
          groupsPrefix   = p.groups_prefix
          scopes         = p.scopes
        },
        p.certificate_authority == null ? {} : { certificateAuthority = p.certificate_authority },
      )]
      backupTargets = [for t in var.backup_targets : merge(
        { name = t.name, bucket = t.bucket, checksums = t.checksums },
        t.endpoint == null ? {} : { endpoint = t.endpoint },
        t.region == null ? {} : { region = t.region },
        t.prefix == null ? {} : { prefix = t.prefix },
        t.ca_certificate == null ? {} : { caCertificate = t.ca_certificate },
      )]
    },
    { image = local.controller_image },
  ))]

  depends_on = [helm_release.cluster_api_providers, proxmox_acl.provisioner, proxmox_acl.ha, proxmox_acl.template_builder, proxmox_acl.etcd_restore]
}
