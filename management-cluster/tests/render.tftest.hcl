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
  # A checkout's chart names no controller image; a release's names its own.
  controller_image = "ghcr.io/kubehenge/kubehenge@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
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
    condition = (
      helm_release.cluster_api_operator.chart == "./charts/cluster-api-operator-${trimprefix(local.capi_operator.chart, "v")}.tgz" &&
      helm_release.cluster_api_operator.repository == null
    )
    error_message = "the Cluster API operator must come from the chart archive beside the module, which its SHA-256 checks, not a Helm repository"
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
      "/access/groups/kubehenge-clusters-audit kubehenge-provisioner-users",
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
      toset(proxmox_virtual_environment_role.cluster_vms.privileges) == toset(["VM.Audit", "VM.Clone", "VM.Config.Disk"])
    )
    error_message = "clusters may use space on the workload storages and configure and copy their own VMs' disks, nothing more"
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
        kubehenge-talos   = ["talos-bootstrap-system", "talos-control-plane-system"]
      }
      # Talos workload clusters require the signatures the management cluster's nodes do, but for
      # those of the providers only the management cluster runs.
      talos = { imageSignatures = [
        { image = "factory.talos.dev/*", keyless = { issuer = "https://accounts.google.com", subject = "image-factory-signing@talos-production.iam.gserviceaccount.com" } },
        { image = "ghcr.io/siderolabs/*", keyless = { issuer = "https://accounts.google.com", subjectRegex = "^(releasemgr-svc@talos-production\\.iam\\.gserviceaccount\\.com|[^@]+@siderolabs\\.com)$" } },
        { image = "registry.k8s.io/*", keyless = { issuer = "https://accounts.google.com", subject = "krel-trust@k8s-releng-prod.iam.gserviceaccount.com" } },
      ] }
      interface = { extraNamespaces = [], offerHARules = false, headlampSignIn = false, headlampActions = false }
      clusters  = { storages = ["local-lvm", "ceph"], bridges = ["vmbr0", "vmbr1"] }
      proxmox = {
        url          = "https://pve.example.org:8006"
        insecure     = false
        secret       = "kubehenge-proxmox"
        clusterGroup = "kubehenge-clusters"
        clusterRole  = "kubehenge-cluster-vms"
        ha           = false
        etcdRestore  = false
        datastoreAllocate = {
          role               = ""
          managementStorages = ["local-lvm", "local"]
        }
        sysAuditGroup = "kubehenge-clusters-audit"
      }
      templateRefresh = {
        enabled       = true
        weekday       = "Sun"
        time          = "01:00"
        timeZone      = "UTC"
        soakHours     = 24
        secret        = "kubehenge-template-builder"
        node          = "pve1"
        storage       = "local-lvm"
        importStorage = "local"
        isoStorage    = "local"
        pool          = "kubehenge-templates"
        bridge        = "vmbr0"
        vlan          = 0
        address       = ""
        gateway       = ""
        dns           = []
        creatorBuilds = false
      }
      clusterOIDC        = []
      backupTargets      = []
      image              = "ghcr.io/kubehenge/kubehenge@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
      managementEndpoint = "https://192.0.2.10:6443"
    }
    error_message = "the Kubehenge chart's values differ"
  }

  assert {
    condition = (
      toset(keys(proxmox_acl.template_builder)) == toset([
        "/ kubehenge-audit",
        "/nodes kubehenge-audit",
        "/nodes/pve1 kubehenge-template-builder-network",
        "/pool/kubehenge-templates kubehenge-template-builder-vms",
        "/sdn/zones/localnetwork/vmbr0 PVESDNUser",
        "/storage/local kubehenge-template-builder-files",
        "/storage/local-lvm kubehenge-cluster-storage",
      ]) &&
      alltrue([for a in proxmox_acl.template_builder : a.user_id == "kubehenge-template-builder@pve"]) &&
      proxmox_virtual_environment_user.template_builder[0].password == null &&
      !proxmox_user_token.template_builder[0].privileges_separation &&
      toset(proxmox_virtual_environment_role.template_builder_network[0].privileges) == toset(["Sys.AccessNetwork", "Sys.Audit"]) &&
      !contains(proxmox_virtual_environment_role.template_builder_vms[0].privileges, "VM.Clone")
    )
    error_message = "the template builder's ACLs differ from the least-privilege set"
  }

  assert {
    condition     = contains(proxmox_virtual_environment_role.template_builder_vms[0].privileges, "Pool.Audit")
    error_message = "the template builder cannot see which VMs are in its pool without Pool.Audit there"
  }

  assert {
    condition = (
      contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-template-builder.data.token") &&
      contains(output.proxmox_objects.users, "kubehenge-template-builder@pve")
    )
    error_message = "the template refresh gets its own token, kept out of the plain values, and a destroy is checked for its user"
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
    condition = [
      for d in split("---\n", data.talos_machine_configuration.node[0].config_patches[1]) : yamldecode(d).kubeNetworkPoliciesEnabled
      if yamldecode(d).kind == "KubeFlannelCNIConfig"
    ] == [true]
    error_message = "the management cluster must enforce NetworkPolicies"
  }

  assert {
    condition = [
      for d in split("---\n", data.talos_machine_configuration.node[0].config_patches[1]) : [
        for r in yamldecode(d).rules : [r.image, r.keyless.issuer, try(r.keyless.subject, r.keyless.subjectRegex)]
      ] if yamldecode(d).kind == "ImageVerificationConfig"
      ] == [[
        ["factory.talos.dev/*", "https://accounts.google.com", "image-factory-signing@talos-production.iam.gserviceaccount.com"],
        ["ghcr.io/siderolabs/*", "https://accounts.google.com", "^(releasemgr-svc@talos-production\\.iam\\.gserviceaccount\\.com|[^@]+@siderolabs\\.com)$"],
        ["registry.k8s.io/*", "https://accounts.google.com", "krel-trust@k8s-releng-prod.iam.gserviceaccount.com"],
        ["ghcr.io/kubehenge/cluster-api-bootstrap-provider-talos", "https://token.actions.githubusercontent.com", "^https://github\\.com/kubehenge/cluster-api-bootstrap-provider-talos/\\.github/workflows/release\\.yaml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.]+)?$"],
        ["ghcr.io/kubehenge/cluster-api-control-plane-provider-talos", "https://token.actions.githubusercontent.com", "^https://github\\.com/kubehenge/cluster-api-control-plane-provider-talos/\\.github/workflows/release\\.yaml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.]+)?$"],
    ]]
    error_message = "Talos must pull the Image Factory's, Sidero's and Kubernetes' images, and the Talos providers' from Kubehenge's forks, only with their publishers' signatures"
  }

  assert {
    condition = (
      length(proxmox_acl.ha) == 0 && length(proxmox_user_token.ha) == 0 &&
      !contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox-ha.data.token") &&
      !contains(keys(yamldecode(helm_release.cluster_api_providers.values[0]).secrets), "kubehenge-proxmox-ha")
    )
    error_message = "no HA token unless Proxmox VE HA is asked for"
  }

  assert {
    condition     = length(proxmox_virtual_environment_role.cluster_datastore) == 0
    error_message = "no cluster may hold Datastore.Allocate unless the administrator lets clusters choose it"
  }

  assert {
    condition = (
      length(proxmox_acl.etcd_restore) == 0 && length(proxmox_user_token.etcd_restore) == 0 &&
      !contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox-restore.data.token") &&
      !contains(keys(yamldecode(helm_release.cluster_api_providers.values[0]).secrets), "kubehenge-proxmox-restore") &&
      !yamldecode(helm_release.kubehenge.values[0]).proxmox.etcdRestore
    )
    error_message = "no restore token unless restores through the guest agent are asked for"
  }
}

run "cluster_sys_audit" {
  command = plan

  assert {
    condition = (
      proxmox_virtual_environment_group.clusters_audit[0].group_id == "kubehenge-clusters-audit" &&
      proxmox_acl.clusters_audit[0].group_id == "kubehenge-clusters-audit" &&
      proxmox_acl.clusters_audit[0].path == "/" && !proxmox_acl.clusters_audit[0].propagate &&
      proxmox_acl.clusters_audit[0].role_id == "kubehenge-audit" &&
      toset(proxmox_virtual_environment_role.audit.privileges) == toset(["Sys.Audit"])
    )
    error_message = "the group holds Sys.Audit on / alone, unpropagated"
  }

  assert {
    condition = (
      toset([for k, a in proxmox_acl.provisioner : k if startswith(a.path, "/access/groups/")]) == toset([
        "/access/groups/kubehenge-clusters kubehenge-provisioner-users",
        "/access/groups/kubehenge-clusters-audit kubehenge-provisioner-users",
      ]) &&
      !proxmox_acl.provisioner["/access/groups/kubehenge-clusters-audit kubehenge-provisioner-users"].propagate &&
      length([for k, a in proxmox_acl.provisioner : k if a.path == "/" && a.role_id != "kubehenge-audit"]) == 0
    )
    error_message = "the controller may change the group's members, and gains nothing on / itself"
  }

  assert {
    condition = (
      length([for k, a in proxmox_acl.clusters : k if a.path == "/"]) == 0 &&
      yamldecode(helm_release.kubehenge.values[0]).proxmox.sysAuditGroup == "kubehenge-clusters-audit"
    )
    error_message = "the clusters' group gets nothing more; the controller learns the group"
  }

  assert {
    condition     = contains(output.proxmox_objects.groups, "kubehenge-clusters-audit")
    error_message = "a destroy is checked for the group too"
  }
}

run "cluster_sys_audit_off" {
  command = plan

  variables {
    cluster_sys_audit = false
  }

  assert {
    condition = (
      length(proxmox_virtual_environment_group.clusters_audit) == 0 && length(proxmox_acl.clusters_audit) == 0 &&
      !contains(keys(proxmox_acl.provisioner), "/access/groups/kubehenge-clusters-audit kubehenge-provisioner-users") &&
      yamldecode(helm_release.kubehenge.values[0]).proxmox.sysAuditGroup == "" &&
      !contains(output.proxmox_objects.groups, "kubehenge-clusters-audit")
    )
    error_message = "switched off, no cluster may hold Sys.Audit on / and the controller may add no one to a group for it"
  }
}

run "etcd_restore" {
  command = plan

  variables {
    etcd_restore = true
  }

  assert {
    condition = (
      toset(proxmox_virtual_environment_role.etcd_restore[0].privileges) == toset(["VM.Audit", "VM.GuestAgent.Unrestricted"]) &&
      proxmox_acl.etcd_restore[0].path == "/pool/${var.workload_pool}" && proxmox_acl.etcd_restore[0].propagate &&
      proxmox_acl.etcd_restore[0].user_id == "kubehenge-etcd-restore@pve" &&
      proxmox_virtual_environment_user.etcd_restore[0].password == null &&
      !proxmox_user_token.etcd_restore[0].privileges_separation
    )
    error_message = "the restore token holds VM.Audit and VM.GuestAgent.Unrestricted on the workload pool alone, for a password-less user of its own"
  }

  assert {
    condition = (
      contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox-restore.data.token") &&
      !contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox.data.restore-token") &&
      yamldecode(helm_release.kubehenge.values[0]).proxmox.etcdRestore
    )
    error_message = "the restore token goes into a Secret of its own for the chart's token Jobs, not the controller's, kept out of the plain values"
  }

  assert {
    condition     = contains(output.proxmox_objects.users, "kubehenge-etcd-restore@pve") && contains(output.proxmox_objects.roles, "kubehenge-etcd-restore")
    error_message = "a destroy is checked for the restore user and role too"
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
      contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox-ha.data.token") &&
      !contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-proxmox.data.ha-token") &&
      yamldecode(helm_release.kubehenge.values[0]).proxmox.ha
    )
    error_message = "the HA token goes into a Secret of its own for the chart's token Jobs, not the controller's, kept out of the plain values"
  }

  assert {
    condition     = contains(output.proxmox_objects.users, "kubehenge-ha@pve") && contains(output.proxmox_objects.roles, "kubehenge-ha")
    error_message = "a destroy is checked for the HA user and role too"
  }
}

run "offer_ha_rules" {
  command = plan

  variables {
    offer_ha_rules = true
  }

  assert {
    condition     = yamldecode(helm_release.kubehenge.values[0]).interface.offerHARules
    error_message = "the chart is told to offer the HA rules"
  }

  assert {
    condition = (
      length(proxmox_acl.ha) == 0 && length(proxmox_user_token.ha) == 0 &&
      !proxmox_acl.provisioner["/ kubehenge-audit"].propagate &&
      proxmox_acl.provisioner["/ kubehenge-audit"].user_id == "kubehenge-provisioner@pve" &&
      toset(proxmox_virtual_environment_role.audit.privileges) == toset(["Sys.Audit"])
    )
    error_message = "offering the HA rules asks for no token, and the controller's own holds Sys.Audit on / without propagation"
  }
}

run "cluster_datastore_allocate" {
  command = plan

  variables {
    cluster_datastore_allocate = true
    workload_datastores        = ["local-lvm", "ceph", "local"]
  }

  assert {
    condition = (
      toset(proxmox_virtual_environment_role.cluster_datastore[0].privileges) == toset(["Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.Audit"]) &&
      toset([for k, a in proxmox_acl.provisioner : k if a.role_id == "kubehenge-cluster-datastore"]) == toset(["/storage/ceph kubehenge-cluster-datastore"]) &&
      !proxmox_acl.provisioner["/storage/ceph kubehenge-cluster-datastore"].propagate
    )
    error_message = "the controller may grant Datastore.Allocate on the workload storages the management cluster does not use, and on no other"
  }

  assert {
    condition = (
      length([for k, a in proxmox_acl.clusters : k if a.role_id == "kubehenge-cluster-datastore"]) == 0 &&
      yamldecode(helm_release.kubehenge.values[0]).proxmox.datastoreAllocate == { role = "kubehenge-cluster-datastore", managementStorages = ["local-lvm", "local"] }
    )
    error_message = "the clusters' group gets nothing more; the controller learns the role and the management cluster's storages"
  }

  assert {
    condition     = contains(output.proxmox_objects.roles, "kubehenge-cluster-datastore")
    error_message = "a destroy is checked for the role too"
  }
}

run "template_refresh_on_dedicated_storages" {
  command = plan

  variables {
    template_refresh = {
      node             = "pve2"
      storage          = "ceph"
      import_datastore = "images"
      iso_datastore    = "seeds"
      address          = "192.0.2.50/24"
      gateway          = "192.0.2.1"
      creator_builds   = true
    }
  }

  assert {
    condition = toset(keys(proxmox_acl.template_builder)) == toset([
      "/ kubehenge-audit",
      "/nodes kubehenge-audit",
      "/nodes/pve2 kubehenge-template-builder-network",
      "/pool/kubehenge-templates kubehenge-template-builder-vms",
      "/sdn/zones/localnetwork/vmbr0 PVESDNUser",
      "/storage/images kubehenge-template-builder-files",
      "/storage/seeds kubehenge-template-builder-files",
      "/storage/ceph kubehenge-cluster-storage",
    ])
    error_message = "full control goes to the import and ISO storages alone; the template disks' storage gets space"
  }

  assert {
    condition = (
      yamldecode(helm_release.kubehenge.values[0]).templateRefresh.address == "192.0.2.50/24" &&
      yamldecode(helm_release.kubehenge.values[0]).templateRefresh.creatorBuilds
    )
    error_message = "the chart builds with the network and creator builds asked for"
  }

  assert {
    condition     = yamldecode(helm_release.kubehenge.values[0]).proxmox.datastoreAllocate.managementStorages == ["local-lvm", "local", "images", "seeds"]
    error_message = "no cluster may hold Datastore.Allocate on the storages the builds download and upload to"
  }
}

run "template_refresh_on_one_storage" {
  command = plan

  variables {
    template_refresh = {
      storage          = "local"
      import_datastore = "local"
      iso_datastore    = "local"
    }
  }

  assert {
    condition = toset(keys(proxmox_acl.template_builder)) == toset([
      "/ kubehenge-audit",
      "/nodes kubehenge-audit",
      "/nodes/pve1 kubehenge-template-builder-network",
      "/pool/kubehenge-templates kubehenge-template-builder-vms",
      "/sdn/zones/localnetwork/vmbr0 PVESDNUser",
      "/storage/local kubehenge-template-builder-files",
      "/storage/local kubehenge-cluster-storage",
    ])
    error_message = "a storage holding the images and the template disks gets space beside full control"
  }
}

run "template_refresh_off" {
  command = plan

  variables {
    template_refresh = { enabled = false }
  }

  assert {
    condition = (
      length(proxmox_acl.template_builder) == 0 && length(proxmox_user_token.template_builder) == 0 &&
      !contains([for s in helm_release.cluster_api_providers.set_sensitive : s.name], "secrets.kubehenge-template-builder.data.token") &&
      !yamldecode(helm_release.kubehenge.values[0]).templateRefresh.enabled
    )
    error_message = "no template builder unless the refresh is on"
  }
}

run "extra_cluster_namespaces" {
  command = plan

  variables {
    extra_cluster_namespaces = ["team-a", "team-b"]
  }

  assert {
    condition = (
      yamldecode(helm_release.kubehenge.values[0]).interface == { extraNamespaces = ["team-a", "team-b"], offerHARules = false, headlampSignIn = false, headlampActions = false } &&
      toset(keys(proxmox_acl.provisioner)) == toset([
        "/ kubehenge-audit",
        "/nodes kubehenge-audit",
        "/pool/kubehenge kubehenge-provisioner-pools",
        "/access/groups/kubehenge-clusters kubehenge-provisioner-users",
        "/access/groups/kubehenge-clusters-audit kubehenge-provisioner-users",
        "/access/realm/pve kubehenge-provisioner-realm",
        "/pool/kubehenge-templates kubehenge-templates-audit",
        "/storage/local-lvm kubehenge-cluster-storage",
        "/storage/ceph kubehenge-cluster-storage",
      ])
    )
    error_message = "the chart is told the namespaces, and Proxmox VE grants nothing more for them: each cluster's pool still nests under the workload pool"
  }
}

run "extra_cluster_namespaces_refuse_system_ones" {
  command = plan

  variables {
    extra_cluster_namespaces = ["team-a", "capi-system"]
  }

  expect_failures = [var.extra_cluster_namespaces]
}

run "extra_cluster_namespaces_refuse_the_interface_s_own" {
  command = plan

  variables {
    extra_cluster_namespaces = ["clusters"]
  }

  expect_failures = [var.extra_cluster_namespaces]
}

run "providers" {
  command = plan

  assert {
    condition = toset([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers : "${p.kind}/${p.name}"
      ]) == toset([
      "CoreProvider/cluster-api",
      "BootstrapProvider/kubeadm", "BootstrapProvider/rke2", "BootstrapProvider/k3s", "BootstrapProvider/talos",
      "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/rke2", "ControlPlaneProvider/k3s", "ControlPlaneProvider/talos",
      "InfrastructureProvider/proxmox", "IPAMProvider/in-cluster", "AddonProvider/helm",
    ])
    error_message = "the provider set differs from Cluster API core, kubeadm, RKE2, k3s, Talos, capmox, in-cluster IPAM and Helm"
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
      "BootstrapProvider/kubeadm", "BootstrapProvider/rke2", "BootstrapProvider/k3s", "BootstrapProvider/talos",
      "ControlPlaneProvider/kubeadm", "ControlPlaneProvider/rke2", "ControlPlaneProvider/k3s", "ControlPlaneProvider/talos",
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
    error_message = "every provider that takes lease flags gets the long leases and no lease name; k3s, Talos and in-cluster IPAM take none"
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
    condition = alltrue([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      can(p.spec.deployment.containers[0].resources.requests.memory) && !can(p.spec.deployment.containers[0].resources.limits)
    ])
    error_message = "every provider gets a memory request, and no limit"
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
    condition = (
      helm_release.cert_manager.repository == "oci://quay.io/jetstack/charts" &&
      can(regex("^cert-manager@sha256:[0-9a-f]{64}$", helm_release.cert_manager.chart)) &&
      alltrue([for v in [
        yamldecode(helm_release.cert_manager.values[0]),
        yamldecode(helm_release.cert_manager.values[0]).cainjector,
        yamldecode(helm_release.cert_manager.values[0]).webhook,
        yamldecode(helm_release.cert_manager.values[0]).startupapicheck,
        yamldecode(helm_release.cert_manager.values[0]).acmesolver,
      ] : can(regex("^v[^@]+@sha256:[0-9a-f]{64}$", v.image.tag))])
    )
    error_message = "cert-manager's chart is pulled by its digest, and each of its images named by one"
  }

  assert {
    condition     = can(regex("^v[^@]+@sha256:[0-9a-f]{64}$", yamldecode(helm_release.cluster_api_operator.values[0]).image.manager.tag))
    error_message = "the operator's image is named by its digest"
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
# Their versions and digests are placeholders there, so a dependency update changes only the module.
run "providers_values_match_the_integration_tier" {
  command = apply

  variables {
    controller_image = "registry.example/kubehenge@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  }

  assert {
    condition = { for k, v in yamldecode(replace(replace(helm_release.cluster_api_providers.values[0],
      "/v[0-9]+\\.[0-9]+\\.[0-9]+/", "v0.0.0"),
      "/[0-9a-f]{64}/", "0000000000000000000000000000000000000000000000000000000000000000")) : k => v if k != "chartDigest"
    } == yamldecode(file("../pkg/provisioner/testdata/cluster-api-providers-values.yaml"))
    error_message = "pkg/provisioner/testdata/cluster-api-providers-values.yaml differs from the module's values for the providers chart; bring it up to date"
  }
}

run "registry_mirrors_and_controller_image" {
  command = apply

  variables {
    registry_mirrors = { "ghcr.io" = ["http://192.0.2.5:5000", "https://mirror.example.org"] }
    controller_image = "ghcr.io/kubehenge/kubehenge@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
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
    condition = (yamldecode(helm_release.kubehenge.values[0]).image == "ghcr.io/kubehenge/kubehenge@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" &&
    yamldecode(helm_release.cluster_api_providers.values[0]).image == "ghcr.io/kubehenge/kubehenge@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
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

run "headlamp_sign_in" {
  command = plan

  variables {
    headlamp_sign_in = true
  }

  assert {
    condition     = yamldecode(helm_release.kubehenge.values[0]).interface == { extraNamespaces = [], offerHARules = false, headlampSignIn = true, headlampActions = false }
    error_message = "the chart must be told to have the shared Headlamp sign people in, its namespaces left as they are"
  }
}

run "headlamp_actions" {
  command = plan

  variables {
    headlamp_actions = true
  }

  assert {
    condition     = yamldecode(helm_release.kubehenge.values[0]).interface == { extraNamespaces = [], offerHARules = false, headlampSignIn = false, headlampActions = true }
    error_message = "the chart must be told to have the shared Headlamp offer its actions, nothing else of the interface changed"
  }
}

run "cluster_oidc_needs_https" {
  command = plan

  variables {
    cluster_oidc = [{ issuer_url = "http://idp.example", client_id = "kubernetes" }]
  }

  expect_failures = [var.cluster_oidc]
}

run "backup_targets" {
  command = apply

  variables {
    backup_targets = [
      { name = "s3", endpoint = "https://s3.example:8333", bucket = "kubehenge", prefix = "site-a", ca_certificate = "-----BEGIN CERTIFICATE-----\n", checksums = false },
      { name = "aws", bucket = "backups", region = "eu-central-2" },
    ]
  }

  assert {
    condition = yamldecode(helm_release.kubehenge.values[0]).backupTargets == [
      { name = "s3", endpoint = "https://s3.example:8333", bucket = "kubehenge", prefix = "site-a", caCertificate = "-----BEGIN CERTIFICATE-----\n", checksums = false },
      { name = "aws", bucket = "backups", region = "eu-central-2", checksums = true },
    ]
    error_message = "the targets must reach the chart's backupTargets in its own names, checksums on unless switched off"
  }
}

run "backup_targets_need_https_and_names_of_their_own" {
  command = plan

  variables {
    backup_targets = [
      { name = "s3", endpoint = "http://s3.example:8333", bucket = "kubehenge" },
      { name = "s3", bucket = "other" },
    ]
  }

  expect_failures = [var.backup_targets]
}

run "controller_image_needs_a_digest" {
  command = plan

  variables {
    controller_image = "ghcr.io/kubehenge/kubehenge:v0.1.0"
  }

  expect_failures = [var.controller_image]
}

run "a_checkout_needs_a_controller_image" {
  command = plan

  variables {
    controller_image = null
  }

  expect_failures = [helm_release.cluster_api_providers]
}

run "provider_image" {
  command = apply

  variables {
    provider_images = {
      "InfrastructureProvider/proxmox" = "registry.example/capmox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      "CoreProvider/cluster-api"       = "registry.example/capi@sha256:456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123"
    }
  }

  assert {
    condition = {
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      "${p.kind}/${p.name}" => p.spec.deployment.containers
      if contains(["InfrastructureProvider/proxmox", "CoreProvider/cluster-api", "AddonProvider/helm"], "${p.kind}/${p.name}")
      } == {
      "InfrastructureProvider/proxmox" = [{
        name      = "manager"
        imageUrl  = "registry.example/capmox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        resources = { requests = { cpu = "10m", memory = "64Mi" } }
      }]
      "CoreProvider/cluster-api" = [{
        name      = "manager"
        imageUrl  = "registry.example/capi@sha256:456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123"
        resources = { requests = { cpu = "10m", memory = "128Mi" } }
      }]
      "AddonProvider/helm" = [{
        name      = "manager"
        imageUrl  = local.helm_addon_build.image
        resources = { requests = { cpu = "10m", memory = "64Mi" } }
      }]
    }
    error_message = "the named providers run the given images, in their manager containers, keeping their requests, and the others their tested ones or Kubehenge's builds"
  }
}

run "provider_builds" {
  command = apply

  assert {
    condition = {
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      "${p.kind}/${p.name}" => p.spec.deployment.containers[0].imageUrl
      } == merge(local.tested_provider_images, {
        "InfrastructureProvider/proxmox" = local.capmox_build.image
        "AddonProvider/helm"             = local.helm_addon_build.image
    })
    error_message = "capmox and the Add-on Provider for Helm at their tested releases run Kubehenge's builds of them, and every other provider its tested image by digest"
  }
}

run "providers_install_from_checked_manifests" {
  command = apply

  assert {
    condition = alltrue([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      p.spec.fetchConfig == { selector = { matchLabels = {
        "kubehenge.dev/provider-manifests"  = lower("${p.kind}-${p.name}")
        "provider.cluster.x-k8s.io/version" = p.spec.version
      } } }
    ])
    error_message = "every provider must come from the ConfigMap the chart's hook stores its checked files in"
  }

  assert {
    condition = [
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers : p.manifests if p.kind == "CoreProvider"
      ] == [{
        metadata   = "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.3/metadata.yaml@sha256:${local.tested_manifests["kubernetes-sigs/cluster-api/releases/download/v1.14.3/metadata.yaml"]}"
        components = "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.3/core-components.yaml@sha256:${local.tested_manifests["kubernetes-sigs/cluster-api/releases/download/v1.14.3/core-components.yaml"]}"
    }]
    error_message = "a provider's files must be its release's, at its version, each with its digest"
  }
}

run "provider_builds_only_on_their_releases" {
  command = apply

  variables {
    version_overrides = { capmox = "v0.9.0", helm_addon = "v0.6.3" }
    provider_images = {
      "InfrastructureProvider/proxmox" = "ghcr.io/ionos-cloud/cluster-api-provider-proxmox:v0.9.0@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      "AddonProvider/helm"             = "registry.k8s.io/cluster-api-helm/cluster-api-helm-controller:v0.6.3@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    }
    provider_manifests = {
      "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.0/metadata.yaml"                  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.0/infrastructure-components.yaml" = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      "kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.3/metadata.yaml"           = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      "kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.3/addon-components.yaml"   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    }
  }

  assert {
    condition = {
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      "${p.kind}/${p.name}" => p.spec.deployment.containers[0].imageUrl
      if contains(["InfrastructureProvider/proxmox", "AddonProvider/helm"], "${p.kind}/${p.name}")
    } == var.provider_images
    error_message = "other releases of capmox and the Add-on Provider for Helm run the images given for them"
  }
}

run "a_provider_held_back_needs_its_image_and_digests" {
  command = plan

  variables {
    version_overrides = { capmox = "v0.9.0" }
  }

  expect_failures = [helm_release.cluster_api_providers]
}

run "provider_image_unknown" {
  command = plan

  variables {
    provider_images = { "InfrastructureProvider/vsphere" = "registry.example/capv@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" }
  }

  expect_failures = [var.provider_images]
}

run "provider_image_needs_a_digest" {
  command = plan

  variables {
    provider_images = { "InfrastructureProvider/proxmox" = "registry.example/capmox:v0.9.0" }
  }

  expect_failures = [var.provider_images]
}

run "provider_manifests_need_a_digest" {
  command = plan

  variables {
    provider_manifests = { "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.0/metadata.yaml" = "sha256:0123" }
  }

  expect_failures = [var.provider_manifests]
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

run "proxmox_insecure" {
  command = plan

  variables {
    proxmox_insecure = true
  }

  assert {
    condition = alltrue([
      for p in yamldecode(helm_release.cluster_api_providers.values[0]).providers :
      contains([for op in yamldecode(p.spec.patches[0].patch) : op.value if can(tostring(op.value))], "--proxmox-insecure=true")
      if p.name == "proxmox"
    ])
    error_message = "capmox skips verifying the Proxmox VE certificate when told to"
  }

  assert {
    condition     = yamldecode(helm_release.kubehenge.values[0]).proxmox.insecure
    error_message = "the controller, and through it the clusters, skip verifying the Proxmox VE certificate when told to"
  }
}

run "rejects_a_node_twice" {
  command = plan

  variables {
    proxmox_nodes = ["pve1", "pve1"]
  }

  expect_failures = [var.proxmox_nodes]
}
