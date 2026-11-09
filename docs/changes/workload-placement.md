### Added

- The create wizard says that volumes on node-local storage stay on one node. The docs cover moving one and the "volume node affinity conflict" error.
- The create wizard warns before submit when no node it may use has the free memory. A bridge missing on the allowed nodes names the nodes that have it.

### Changed

- A cluster's bridge or VNet must exist on a node it may use, or the cluster is refused. Placement leaves out the nodes without it.
- The interface marks `NodesSkipped` and `NodesLackMemory` as problems while they are true.

### Fixed

- New machines are no longer refused with "cannot reserve … of memory" on an overcommitted host. Placement skips nodes without room for the largest machine, and sets `NodesLackMemory` when none has it.
