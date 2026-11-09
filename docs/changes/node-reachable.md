### Added

- `NodesSkipped`, the management Health page, the create wizard and the pre-flight name an unreachable node with its status and reason.
- [Troubleshooting](troubleshooting.md#a-node-is-online-but-not-reachable) has the fix for a node that is online but not reachable.
- A workload cluster's Health page lists such a node as a warning with its fix, and the fleet and Health tab count it.
- The interface shows a command in a message from the controller as code.

### Changed

- A Proxmox VE node counts as available only when a call forwarded to it through the cluster answers.
- Placement, template builds and volume owners skip a node that is online but answers HTTP 596 or takes over 10 seconds.
- The controller reads each online node's status at every look, all nodes at once, instead of once while the node stays online.
