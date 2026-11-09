### Added

- A failure creating a cluster's Proxmox VE pool, user, token or volume owner records a `ProxmoxObjectsFailed` warning event with the error.
- `--node` of `kubehenge template create` and `refresh`, and the chart's `templateRefresh.node`, take several nodes. The build runs on the first that is online and whose CPU runs the node OS.

### Changed

- Clusters run only on the nodes that can clone their template, as one on unshared storage clones only on its own node. With none allowed, the refusal names the template, its storage and the fix.
- `kubehenge template create` and the build dialog say when the template's storage is not shared. Without `--storage`, the command takes the shared storage with the most room, else asks.
- Placement leaves out nodes where the cluster's storage is unavailable, and needs one online node that has it.
- A template build checks its storages' content types and its own `Datastore.AllocateSpace` before downloading. Without `--iso-storage` it takes the first ISO storage the token can write to.
- Template builds and new clusters check the datacenter's `user-tag-access` and `registered-tags` first, as Proxmox VE does. Tags match with their case, and only `Sys.Modify` on `/` bypasses the options.
- Applying a Cluster waits at most 2 seconds for Proxmox VE. When it is slower, the storage and template checks run as the cluster is created.

### Fixed

- A cluster storage outside the management cluster's `workload_datastores` is refused, naming the setting to add it to, where the clusters' tokens failed with a permission error.
- A Proxmox VE node that shows online but whose status cannot be read is left out of placement and counted in `NodesSkipped`, where it stopped every cluster from being placed.
- A cluster whose name matches a Proxmox VE pool holding other VMs is refused instead of taking the pool over.
