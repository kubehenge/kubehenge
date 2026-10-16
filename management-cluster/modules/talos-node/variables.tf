variable "node" {
  description = "IP address of the node."
  type        = string
}

variable "client_configuration" {
  description = "Talos client credentials of the cluster."
  type = object({
    ca_certificate     = string
    client_certificate = string
    client_key         = string
  })
  sensitive = true
}

variable "machine_configuration" {
  description = "The node's machine configuration."
  type        = string
  sensitive   = true
}

variable "image" {
  description = "Talos installer image; a different one upgrades the node."
  type        = string
}

variable "drain_on_upgrade" {
  description = "Drain the node before rebooting it for an upgrade."
  type        = bool
}

variable "kubeconfig" {
  description = "Kubeconfig for draining; needed when drain_on_upgrade is true."
  type        = string
  default     = null
  ephemeral   = true
}
