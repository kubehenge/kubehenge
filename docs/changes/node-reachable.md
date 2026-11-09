### Added

- The management Health page, the create wizard, the pre-flight and `NodesSkipped` name an unreachable node with its status and reason ([Troubleshooting](troubleshooting.md#a-node-is-online-but-not-reachable)).
- A workload cluster's Health page lists such a node as a warning with its fix, and the fleet and Health tab count it.

### Changed

- Placement, template builds and volume owners skip a Proxmox VE node the cluster cannot reach, such as one answering HTTP 596 or taking over 10 seconds.
