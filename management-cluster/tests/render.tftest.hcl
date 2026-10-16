mock_provider "proxmox" {
  mock_resource "proxmox_user_token" {
    defaults = {
      id    = "kubehenge-cluster-api@pve!capmox"
      value = "kubehenge-cluster-api@pve!capmox=00000000-0000-0000-0000-000000000000"
    }
  }
  mock_resource "proxmox_download_file" {
    defaults = {
      id = "local:import/talos-v1.14.4-ce4c980550dd-nocloud-amd64.qcow2"
    }
  }
}

mock_provider "talos" {
  mock_resource "talos_image_factory_schematic" {
    defaults = {
      id = "ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515"
    }
  }
  mock_data "talos_image_factory_urls" {
    defaults = {
      urls = {
        disk_image            = "https://factory.talos.dev/image/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515/v1.14.4/nocloud-amd64.qcow2"
        installer             = "factory.talos.dev/nocloud-installer/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515:v1.14.4"
        disk_image_secureboot = ""
        initramfs             = ""
        installer_secureboot  = ""
        iso                   = ""
        iso_secureboot        = ""
        kernel                = ""
        kernel_command_line   = ""
        pxe                   = ""
        uki                   = ""
      }
    }
  }
}

mock_provider "helm" {}

variables {
  proxmox_endpoint       = "https://pve.example.org:8006"
  proxmox_api_token      = "root@pam!test=secret"
  proxmox_ca_certificate = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
  proxmox_nodes          = ["pve1", "pve2"]
  node_addresses         = ["192.0.2.11/24", "192.0.2.12/24", "192.0.2.13/24"]
  api_address            = "192.0.2.10"
  gateway                = "192.0.2.1"
  dns_servers            = ["192.0.2.1"]
  workload_datastores    = ["local-lvm", "ceph"]
  workload_bridges       = ["vmbr0", "vmbr1"]
}

run "nodes" {
  command = apply

  assert {
    condition     = [for vm in proxmox_virtual_environment_vm.node : vm.name] == ["kubehenge-cp-0", "kubehenge-cp-1", "kubehenge-cp-2"]
    error_message = "control-plane VMs are not named <cluster>-cp-<index>"
  }

  assert {
    condition     = [for vm in proxmox_virtual_environment_vm.node : vm.node_name] == ["pve1", "pve2", "pve1"]
    error_message = "the VMs take the nodes in turn"
  }

  assert {
    condition     = keys(proxmox_download_file.talos) == ["pve1", "pve2"] && alltrue([for vm in proxmox_virtual_environment_vm.node : vm.migrate])
    error_message = "every node gets the Talos image, and a VM moved to another node migrates there"
  }

  assert {
    condition = (
      yamldecode(helm_release.kubehenge.values[0]).clusters.storages == ["local-lvm", "ceph"] &&
      yamldecode(helm_release.kubehenge.values[0]).clusters.bridges == ["vmbr0", "vmbr1"]
    )
    error_message = "the ClusterClasses must know the storages and bridges clusters may use"
  }

  assert {
    condition     = alltrue([for vm in proxmox_virtual_environment_vm.node : vm.pool_id == "kubehenge-management"])
    error_message = "management VMs must sit in the management pool, out of Cluster API's reach"
  }

  assert {
    condition = (
      length(toset([for vm in proxmox_virtual_environment_vm.node : vm.network_device[0].mac_address])) == 3 &&
      alltrue([for vm in proxmox_virtual_environment_vm.node : can(regex("^BC:24:11(:[0-9A-F]{2}){3}$", vm.network_device[0].mac_address))])
    )
    error_message = "nodes need distinct MAC addresses in Proxmox VE's range"
  }

  assert {
    condition     = output.api_endpoint == "https://192.0.2.10:6443"
    error_message = "the API endpoint must be the virtual IP"
  }

  assert {
    condition = (
      module.talos_node_0.drain_on_upgrade &&
      module.talos_node_1[0].drain_on_upgrade &&
      module.talos_node_2[0].drain_on_upgrade &&
      length(module.talos_node_3) == 0 &&
      length(module.talos_node_4) == 0
    )
    error_message = "three nodes need three chained Talos node modules, each draining before an upgrade"
  }
}

run "cluster_api_token_is_least_privilege" {
  # Reads what "nodes" applied, as do the runs up to the first with variables of its own.
  command = plan

  assert {
    condition = toset(keys(proxmox_acl.cluster_api)) == toset([
      "/ kubehenge-audit",
      "/nodes kubehenge-audit",
      "/pool/kubehenge PVEVMAdmin",
      "/pool/kubehenge-templates PVETemplateUser",
      "/storage/local PVEDatastoreAdmin",
      "/storage/local-lvm kubehenge-allocate-space",
      "/storage/ceph kubehenge-allocate-space",
      "/sdn/zones/localnetwork/vmbr0 PVESDNUser",
      "/sdn/zones/localnetwork/vmbr1 PVESDNUser",
    ])
    error_message = "the Cluster API token's ACLs differ from the least-privilege set"
  }

  assert {
    condition = (
      alltrue([for a in proxmox_acl.cluster_api : a.user_id == "kubehenge-cluster-api@pve"]) &&
      !proxmox_user_token.cluster_api.privileges_separation &&
      proxmox_virtual_environment_user.cluster_api.password == null
    )
    error_message = "the ACLs belong to a password-less user whose one token carries its privileges"
  }

  assert {
    condition     = proxmox_acl.cluster_api["/pool/kubehenge PVEVMAdmin"].propagate && !proxmox_acl.cluster_api["/pool/kubehenge-templates PVETemplateUser"].propagate
    error_message = "the workload pool's ACL must reach the clusters' nested pools; the template pool's has nothing below it to reach"
  }

  assert {
    condition = (
      helm_release.cluster_api_providers.set_sensitive[0].name == "secrets.capmox-variables.data.PROXMOX_SECRET" &&
      helm_release.cluster_api_providers.set_sensitive[0].value == "00000000-0000-0000-0000-000000000000"
    )
    error_message = "capmox must get the token secret without the token ID"
  }
}

run "controller_and_clusters_are_least_privilege" {
  command = plan

  assert {
    condition = toset(keys(proxmox_acl.provisioner)) == toset([
      "/ kubehenge-audit",
      "/nodes kubehenge-audit",
      "/pool/kubehenge kubehenge-provisioner-pools",
      "/access/groups/kubehenge-clusters kubehenge-provisioner-users",
      "/access/realm/pve kubehenge-provisioner-realm",
      "/pool/kubehenge-templates kubehenge-templates-audit",
      "/storage/local-lvm kubehenge-cluster-storage",
      "/storage/ceph kubehenge-cluster-storage",
    ]) && proxmox_acl.provisioner["/pool/kubehenge kubehenge-provisioner-pools"].propagate
    error_message = "the controller's ACLs differ from the least-privilege set"
  }

  assert {
    condition     = proxmox_virtual_environment_role.templates_audit.privileges == toset(["VM.Audit"])
    error_message = "the controller may only see the templates"
  }

  assert {
    condition = (
      alltrue([for a in proxmox_acl.provisioner : a.user_id == "kubehenge-provisioner@pve"]) &&
      !proxmox_user_token.provisioner.privileges_separation &&
      proxmox_virtual_environment_user.provisioner.password == null
    )
    error_message = "the controller's ACLs belong to a password-less user whose one token carries its privileges"
  }

  assert {
    condition = (
      toset(keys(proxmox_acl.clusters)) == toset([
        "/storage/local-lvm kubehenge-cluster-storage",
        "/storage/ceph kubehenge-cluster-storage",
        "/nodes kubehenge-audit",
      ]) &&
      !contains(proxmox_virtual_environment_role.cluster_storage.privileges, "Datastore.Allocate") &&
      toset(proxmox_virtual_environment_role.cluster_vms.privileges) == toset(["VM.Audit", "VM.Config.Disk"])
    )
    error_message = "clusters may use space on the workload storages and configure their own VMs' disks, nothing more"
  }

  assert {
    condition = (
      yamldecode(helm_release.cluster_api_providers.values[0]).secrets.cluster-api-variables.data.EXP_RUNTIME_SDK == "true" &&
      yamldecode(helm_release.cluster_api_providers.values[0]).secrets.kubehenge-proxmox.data["ca.crt"] == var.proxmox_ca_certificate &&
      contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox.data.token")
    )
    error_message = "the controller needs the Runtime SDK, its token kept out of the plain values, and the CA"
  }

  assert {
    condition = (
      can(regex("^[0-9a-f]{64}$", yamldecode(helm_release.kubehenge.values[0]).chartDigest)) &&
      can(regex("^[0-9a-f]{64}$", yamldecode(helm_release.cluster_api_providers.values[0]).chartDigest)) &&
      yamldecode(helm_release.kubehenge.values[0]).chartDigest != yamldecode(helm_release.cluster_api_providers.values[0]).chartDigest
    )
    error_message = "each release carries its own chart's digest, so a changed chart is upgraded"
  }

  assert {
    condition = { for k, v in yamldecode(helm_release.kubehenge.values[0]) : k => v if k != "chartDigest" } == {
      workloadPool = "kubehenge"
      providerNamespaces = {
        kubehenge-kubeadm = ["kubeadm-bootstrap-system", "kubeadm-control-plane-system"]
        kubehenge-rke2    = ["rke2-bootstrap-system", "rke2-control-plane-system"]
        kubehenge-k3s     = ["k3s-bootstrap-system", "k3s-control-plane-system"]
      }
      clusters = { storages = ["local-lvm", "ceph"], bridges = ["vmbr0", "vmbr1"] }
      proxmox = {
        url          = "https://pve.example.org:8006"
        secret       = "kubehenge-proxmox"
        clusterGroup = "kubehenge-clusters"
        clusterRole  = "kubehenge-cluster-vms"
        ha           = false
      }
      clusterOIDC = []
      image       = "ghcr.io/kubehenge/kubehenge:v${yamldecode(file("../charts/kubehenge/Chart.yaml")).appVersion}"
    }
    error_message = "the Kubehenge chart's values differ"
  }

  assert {
    condition     = yamldecode(helm_release.cluster_api_providers.values[0]).image == yamldecode(helm_release.kubehenge.values[0]).image
    error_message = "the providers' hooks run the controller's image"
  }

  assert {
    condition     = !strcontains(data.talos_machine_configuration.node[0].config_patches[1], "RegistryMirrorConfig")
    error_message = "no registry mirror unless one is asked for"
  }

  assert {
    condition = (
      length(proxmox_acl.ha) == 0 && length(proxmox_user_token.ha) == 0 &&
      !contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox.data.ha-token")
    )
    error_message = "no HA token unless Proxmox VE HA is asked for"
  }
}

run "proxmox_ha" {
  command = plan

  variables {
    proxmox_ha = true
  }

  assert {
    condition = (
      toset(proxmox_virtual_environment_role.ha[0].privileges) == toset(["Sys.Audit", "Sys.Console"]) &&
      proxmox_acl.ha[0].path == "/" && !proxmox_acl.ha[0].propagate &&
      proxmox_acl.ha[0].user_id == "kubehenge-ha@pve" &&
      proxmox_virtual_environment_user.ha[0].password == null &&
      !proxmox_user_token.ha[0].privileges_separation
    )
    error_message = "the HA token holds Sys.Audit and Sys.Console on / alone, for a password-less user of its own"
  }

  assert {
    condition = (
      contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox.data.ha-token") &&
      yamldecode(helm_release.kubehenge.values[0]).proxmox.ha
    )
    error_message = "the controller gets the HA token, kept out of the plain values"
  }

  assert {
    condition     = contains(output.proxmox_objects.users, "kubehenge-ha@pve") && contains(output.proxmox_objects.roles, "kubehenge-ha")
    error_message = "a destroy is checked for the HA user and role too"
  }
}

run "providers" {
  command = plan

  assert {
    condition = toset([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers : "${p.kind}/${p.name}"
      ]) == toset([
      "CoreProvider/cluster-api",
      "BootstrapProvider/kubeadm", "BootstrapProvider/rke2", "BootstrapProvider/k3s",
      "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/rke2", "ControlPlaneProvider/k3s",
      "InfrastructureProvider/proxmox", "IPAMProvider/in-cluster", "AddonProvider/helm",
    ])
    error_message = "the provider set differs from Cluster API core, kubeadm, RKE2, k3s, capmox, in-cluster IPAM and Helm"
  }

  assert {
    condition = alltrue([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      startswith(p.spec.fetchConfig.url, "https://github.com/k3s-io/cluster-api-k3s/releases/") if p.name == "k3s"
    ])
    error_message = "the k3s providers need their fetch URL"
  }

  assert {
    condition = alltrue([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      toset([for op in yamldecode(p.spec.patches[0].patch) : op.value if can(tostring(op.value))]) ==
      toset(["--proxmox-insecure=false", "--proxmox-root-cert-file=/etc/proxmox-ca/ca.crt"])
      if p.name == "proxmox"
    ])
    error_message = "capmox must verify the Proxmox VE certificate"
  }

  assert {
    condition = toset([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers : "${p.kind}/${p.name}"
      if anytrue([for patch in try(p.spec.patches, []) : try(yamldecode(patch.patch).spec.replicas, "set") == null])
      ]) == toset([
      "BootstrapProvider/kubeadm", "BootstrapProvider/rke2", "BootstrapProvider/k3s",
      "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/rke2", "ControlPlaneProvider/k3s",
    ])
    error_message = "the distributions' providers, and only they, must leave their replica count to Kubehenge's controller"
  }

  assert {
    condition = {
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      "${p.kind}/${p.name}" => [
        p.spec.manager.leaderElection.leaseDuration, p.spec.manager.leaderElection.renewDeadline,
        p.spec.manager.leaderElection.retryPeriod, p.spec.manager.leaderElection.resourceName,
      ] if can(p.spec.manager.leaderElection)
      } == { for k in [
        "CoreProvider/cluster-api", "BootstrapProvider/kubeadm", "BootstrapProvider/rke2",
        "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/rke2", "InfrastructureProvider/proxmox", "AddonProvider/helm",
    ] : k => ["60s", "40s", "10s", ""] }
    error_message = "every provider that takes lease flags gets the long leases and no lease name; k3s and in-cluster IPAM take none"
  }

  assert {
    condition = (alltrue([for kind in ["KubeControllerManagerConfig", "KubeSchedulerConfig"] : anytrue([
      for d in split("---\n", data.talos_machine_configuration.node[0].config_patches[1]) :
      try(yamldecode(d).kind == kind && yamldecode(d).extraArgs == {
        leader-elect-lease-duration = "60s", leader-elect-renew-deadline = "40s", leader-elect-retry-period = "10s"
      }, false)
      ])]) &&
      yamldecode(helm_release.cluster_api_operator.values[0]).leaderElection.renewDeadline == "40s" &&
      yamldecode(helm_release.cluster_api_operator.values[0]).profilerAddress == ""
    )
    error_message = "kube-controller-manager, kube-scheduler and the operator get the providers' long leases, and the operator no profiler"
  }

  assert {
    condition = toset([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers : "${p.kind}/${p.name}"
      if can(p.spec.deployment.containers[0].resources.requests.memory) && !can(p.spec.deployment.containers[0].resources.limits)
      ]) == toset([
      "CoreProvider/cluster-api", "BootstrapProvider/kubeadm", "BootstrapProvider/rke2", "BootstrapProvider/k3s",
      "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/k3s", "AddonProvider/helm",
    ])
    error_message = "every provider whose manifests set no requests gets a memory request, and no limit"
  }

  assert {
    condition = alltrue([for v in [
      yamldecode(helm_release.cert_manager.values[0]),
      yamldecode(helm_release.cert_manager.values[0]).cainjector,
      yamldecode(helm_release.cert_manager.values[0]).webhook,
    ] : can(v.resources.requests.memory) && !can(v.resources.limits)])
    error_message = "cert-manager's controller, cainjector and webhook each get a memory request, and no limit"
  }

  assert {
    condition = {
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      "${p.kind}/${p.name}" => p.spec.configSecret.name if can(p.spec.configSecret)
      } == {
      "CoreProvider/cluster-api"       = "cluster-api-variables"
      "ControlPlaneProvider/kubeadm"   = "cluster-api-variables"
      "InfrastructureProvider/proxmox" = "capmox-variables"
    }
    error_message = "only core, kubeadm's control plane and capmox take variables, each from its own secret"
  }

  assert {
    condition = (
      yamldecode(helm_release.cluster_api_providers.values[0]).secrets.cluster-api-variables.data.CLUSTER_TOPOLOGY == "true" &&
      yamldecode(helm_release.cluster_api_providers.values[0]).secrets.capmox-variables.data.ClusterTopology == "true"
    )
    error_message = "ClusterClass needs the ClusterTopology feature in core, kubeadm and capmox"
  }

  assert {
    condition     = !strcontains(helm_release.cluster_api_providers.values[0], "00000000-0000-0000-0000-000000000000")
    error_message = "the token secret must not appear in the release's plain values"
  }

  assert {
    condition     = !can(yamldecode(helm_release.cluster_api_operator.values[0]).configSecret)
    error_message = "a chart-wide configSecret would override every provider's own"
  }
}

# The integration tier installs the providers chart with these values, against the operator's CRDs.
run "providers_values_match_the_integration_tier" {
  command = apply

  variables {
    controller_image = "registry.example/kubehenge:integration"
  }

  assert {
    condition     = { for k, v in yamldecode(helm_release.cluster_api_providers.values[0]) : k => v if k != "chartDigest" } == yamldecode(file("../pkg/provisioner/testdata/cluster-api-providers-values.yaml"))
    error_message = "pkg/provisioner/testdata/cluster-api-providers-values.yaml differs from the module's values for the providers chart; bring it up to date"
  }
}

run "registry_mirrors_and_controller_image" {
  command = apply

  variables {
    registry_mirrors = { "ghcr.io" = ["http://192.0.2.5:5000", "https://mirror.example.org"] }
    controller_image = "ghcr.io/kubehenge/kubehenge@sha256:0123"
  }

  assert {
    condition = [
      for d in split("---\n", data.talos_machine_configuration.node[0].config_patches[1]) : yamldecode(d)
      if yamldecode(d).kind == "RegistryMirrorConfig"
      ] == [{
        apiVersion = "v1alpha1"
        kind       = "RegistryMirrorConfig"
        name       = "ghcr.io"
        endpoints  = [{ url = "http://192.0.2.5:5000" }, { url = "https://mirror.example.org" }]
    }]
    error_message = "registry mirrors must become Talos RegistryMirrorConfig documents, endpoints in order"
  }

  assert {
    condition = (yamldecode(helm_release.kubehenge.values[0]).image == "ghcr.io/kubehenge/kubehenge@sha256:0123" &&
    yamldecode(helm_release.cluster_api_providers.values[0]).image == "ghcr.io/kubehenge/kubehenge@sha256:0123")
    error_message = "the controller image must reach both charts, the providers' for its hooks"
  }
}

run "cluster_oidc" {
  command = apply

  variables {
    cluster_oidc = [
      { issuer_url = "https://idp.example/realms/k8s", client_id = "kubernetes", certificate_authority = "-----BEGIN CERTIFICATE-----\n" },
      { issuer_url = "https://login.example", client_id = "lab", username_claim = "sub", username_prefix = "" },
    ]
  }

  assert {
    condition = yamldecode(helm_release.kubehenge.values[0]).clusterOIDC == [
      {
        issuerURL   = "https://idp.example/realms/k8s", clientID = "kubernetes", usernameClaim = "email", usernamePrefix = "oidc:",
        groupsClaim = "groups", groupsPrefix = "oidc:", scopes = ["email", "profile"], certificateAuthority = "-----BEGIN CERTIFICATE-----\n"
      },
      {
        issuerURL   = "https://login.example", clientID = "lab", usernameClaim = "sub", usernamePrefix = "",
        groupsClaim = "groups", groupsPrefix = "oidc:", scopes = ["email", "profile"]
      },
    ]
    error_message = "the providers must reach the chart's clusterOIDC in its own names, defaults filled in"
  }
}

run "cluster_oidc_needs_https" {
  command = plan

  variables {
    cluster_oidc = [{ issuer_url = "http://idp.example", client_id = "kubernetes" }]
  }

  expect_failures = [var.cluster_oidc]
}

run "provider_image" {
  command = apply

  variables {
    provider_images = {
      "InfrastructureProvider/proxmox" = "registry.example/capmox@sha256:0123"
      "CoreProvider/cluster-api"       = "registry.example/capi@sha256:4567"
    }
  }

  assert {
    condition = {
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      "${p.kind}/${p.name}" => p.spec.deployment.containers if can(p.spec.deployment.containers[0].imageUrl)
      } == {
      "InfrastructureProvider/proxmox" = [{ name = "manager", imageUrl = "registry.example/capmox@sha256:0123" }]
      "CoreProvider/cluster-api" = [{
        name      = "manager"
        imageUrl  = "registry.example/capi@sha256:4567"
        resources = { requests = { cpu = "10m", memory = "128Mi" } }
      }]
    }
    error_message = "only the named providers run the given images, in their manager containers, keeping their requests"
  }
}

run "capmox_build" {
  command = apply

  assert {
    condition = [
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      p.spec.deployment.containers if can(p.spec.deployment.containers[0].imageUrl)
      ] == [[{
        name     = "manager"
        imageUrl = local.capmox_build.image
    }]]
    error_message = "capmox at its tested release runs Kubehenge's build of it, and no other provider an image of its own"
  }
}

run "capmox_build_only_on_its_release" {
  command = apply

  variables {
    version_overrides = { capmox = "v0.9.0" }
  }

  assert {
    condition = alltrue([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      !can(p.spec.deployment.containers[0].imageUrl)
    ])
    error_message = "another capmox release runs its own image"
  }
}

run "provider_image_unknown" {
  command = plan

  variables {
    provider_images = { "InfrastructureProvider/vsphere" = "registry.example/capv:1" }
  }

  expect_failures = [var.provider_images]
}

run "single_node_does_not_drain" {
  command = apply

  variables {
    node_addresses = ["192.0.2.11/24"]
  }

  assert {
    condition     = !module.talos_node_0.drain_on_upgrade && length(module.talos_node_1) == 0
    error_message = "a single node has nowhere to drain to"
  }
}

run "rejects_nodes_inside_the_service_range" {
  command = plan

  variables {
    node_addresses = ["10.100.0.11/24"]
    api_address    = "10.100.0.10"
    gateway        = "10.100.0.1"
  }

  expect_failures = [proxmox_virtual_environment_vm.node]
}

run "rejects_overlapping_pod_and_service_ranges" {
  command = plan

  variables {
    pod_subnets     = ["10.96.0.0/16"]
    service_subnets = ["10.96.0.0/12"]
  }

  expect_failures = [proxmox_virtual_environment_vm.node]
}

run "accepts_nodes_beside_moved_ranges" {
  command = plan

  variables {
    node_addresses  = ["10.100.0.11/24"]
    api_address     = "10.100.0.10"
    gateway         = "10.100.0.1"
    service_subnets = ["10.200.0.0/16"]
  }
}

run "rejects_two_nodes" {
  command = plan

  variables {
    node_addresses = ["192.0.2.11/24", "192.0.2.12/24"]
  }

  expect_failures = [var.node_addresses]
}

run "rejects_a_non_pem_ca" {
  command = plan

  variables {
    proxmox_ca_certificate = "not a certificate"
  }

  expect_failures = [var.proxmox_ca_certificate]
}

run "rejects_a_node_twice" {
  command = plan

  variables {
    proxmox_nodes = ["pve1", "pve1"]
  }

  expect_failures = [var.proxmox_nodes]
}
