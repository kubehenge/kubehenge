### Added

- The create wizard says that volumes on node-local storage stay on one node.
- The docs explain how replacements meet node-local volumes and how to move a volume.
- Troubleshooting covers "volume node affinity conflict".
- The create wizard warns before submit when no node it may use has the free memory, in amber at Placement and in Confirm's checks.
- A bridge missing on the allowed nodes names the nodes that have it.

### Changed

- Placement leaves out nodes whose free memory cannot take the cluster's largest machine.
- With no such node, placement keeps its last result and sets `NodesLackMemory`.
- A cluster's bridge or VNet must exist on a node it may use, or the cluster is refused.
- Placement leaves out the nodes without the cluster's bridge or VNet.
- A new cluster's first machines go only to nodes with room for its largest machine.
- The interface marks `NodesSkipped` and `NodesLackMemory` as problems while they are true.

### Fixed

- New machines are no longer refused with "cannot reserve … of memory" on an overcommitted Proxmox VE host.
