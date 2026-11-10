# The Talos provider is the real one here: its data source applies the module's patches to the
# generated configuration, so the plans of the runs below carry the configuration each node is sent.
# Everything that would reach a network is overridden. talos_test.go, which make unit-tofu runs on
# this test's output, validates those configurations with Talos's own library.
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
mock_provider "helm" {}

override_resource {
  target = talos_image_factory_schematic.this
  values = {
    id = "ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515"
  }
}

override_data {
  target = data.talos_image_factory_urls.this
  values = {
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

override_resource {
  target = talos_cluster.this
}

override_resource {
  target = talos_cluster_kubeconfig.this
}

override_data {
  target = data.talos_cluster_health.this
}

override_resource {
  target = module.talos_node_0.talos_machine.this
}

variables {
  proxmox_endpoint       = "https://pve.example.org:8006"
  proxmox_api_token      = "root@pam!test=secret"
  proxmox_ca_certificate = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
  proxmox_nodes          = ["pve1"]
  node_addresses         = ["192.0.2.11/24"]
  api_address            = "192.0.2.10"
  gateway                = "192.0.2.1"
  dns_servers            = ["192.0.2.1"]
  workload_datastores    = ["local-lvm"]
  workload_bridges       = ["vmbr0"]
  controller_image       = "ghcr.io/kubehenge/kubehenge@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
}

run "setup" {
  command = apply
}

run "ipv4" {
  command = plan
}

run "dual" {
  command = plan

  variables {
    ip_families         = ["IPv4", "IPv6"]
    node_addresses_ipv6 = ["2001:db8:1::11/64"]
    gateway_ipv6        = "2001:db8:1::1"
    dns_servers         = ["192.0.2.1", "2001:db8:1::53"]
  }
}

run "ipv6_first" {
  command = plan

  variables {
    ip_families         = ["IPv6", "IPv4"]
    node_addresses_ipv6 = ["2001:db8:1::11/64"]
    gateway_ipv6        = "2001:db8:1::1"
    api_address         = "2001:db8:1::10"
    dns_servers         = ["2001:db8:1::53", "192.0.2.1"]
  }
}

run "ipv6_only" {
  command = plan

  variables {
    ip_families         = ["IPv6"]
    node_addresses      = []
    gateway             = null
    node_addresses_ipv6 = ["2001:db8:1::11/64"]
    gateway_ipv6        = "2001:db8:1::1"
    api_address         = "2001:db8:1::10"
    dns_servers         = ["2001:db8:1::53"]
  }
}
