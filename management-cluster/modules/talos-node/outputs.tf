output "drain_on_upgrade" {
  description = "Whether the node is drained before an upgrade."
  value       = talos_machine.this.drain_on_upgrade
}
