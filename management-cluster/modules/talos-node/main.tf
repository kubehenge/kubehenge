# One Talos node's configuration and OS version. A module of its own so that the root can chain the
# nodes with depends_on: upgrades then reboot one control-plane node at a time and etcd keeps quorum.

terraform {
  required_version = ">= 1.11"

  required_providers {
    talos = {
      source  = "siderolabs/talos"
      version = "~> 0.12"
    }
  }
}

# A VM made again boots Talos unconfigured at the same address. Its talos_machine keeps the state of
# the VM before, since the refresh cannot reach the node, so only a replacement sends the
# configuration again. Destroying a talos_machine leaves the node alone.
resource "terraform_data" "vm" {
  triggers_replace = var.vm_id
}

resource "talos_machine" "this" {
  node                            = var.node
  client_configuration            = var.client_configuration
  machine_configuration           = var.machine_configuration
  image                           = var.image
  ignore_kubernetes_upgrade_drift = true
  drain_on_upgrade                = var.drain_on_upgrade
  kubeconfig_wo                   = var.drain_on_upgrade ? var.kubeconfig : null

  lifecycle {
    replace_triggered_by = [terraform_data.vm]
  }
}
