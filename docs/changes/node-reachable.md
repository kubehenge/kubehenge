### Added

- `NodesSkipped`, the management Health page, the create wizard and the pre-flight name an unreachable node with its status and reason, and [Troubleshooting](troubleshooting.md#a-node-is-online-but-not-reachable) has the fix.
- A workload cluster's Health page lists such a node as a warning with its fix, and the fleet and Health tab count it.
- The interface shows a command in a message from the controller as code.

### Changed

- A Proxmox VE node counts as available only when a call forwarded through the cluster answers, so placement, template builds and volume owners skip one answering HTTP 596 or taking over 10 seconds.
- The controller reads each online node's status at every look, all nodes at once, instead of once while the node stays online.
