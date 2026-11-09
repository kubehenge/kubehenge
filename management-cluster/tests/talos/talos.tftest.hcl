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
mock_provider "talos" {
  mock_resource "talos_image_factory_schematic" {
    defaults = {
      id = "ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515"
    }
  }
}

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
      disk_image = "https://factory.talos.dev/image/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515/v1.14.4/nocloud-amd64.qcow2"
      installer  = "factory.talos.dev/nocloud-installer/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515:v1.14.4"
    }
  }
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
