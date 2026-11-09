### Added

- A failure creating a cluster's Proxmox VE pool, user, token or volume owner is a `ProxmoxObjectsFailed` Warning Event.
- The creation hook's message for such a failure reads `Proxmox VE objects:` followed by the error.
- `kubehenge template create` and the build dialog say when the template's storage is not shared.
- `--node` of `kubehenge template create` and `refresh`, and the chart's `templateRefresh.node`, take several nodes.
- The build runs on the first of those nodes that is online and whose CPU runs the node OS.

### Changed

- Clusters run only on the nodes that can clone their template, as a template on unshared storage clones only on its own node.
- A cluster whose allowed nodes leave none is refused, with the template, its storage and the fix named.
- Without `--storage`, `kubehenge template create` takes the shared storage with the most room for disk images, else asks for one.
- Placement leaves out nodes where the cluster's storage is unavailable, and needs one online node that has it.
- A cluster storage outside the management cluster's `workload_datastores` is refused, with the setting to add it to.
- A template build checks the import, ISO and disk storages' content types and its own `Datastore.AllocateSpace` before downloading.
- Without `--iso-storage`, a template build takes the first ISO storage the token can write to.
- Placement takes the CPU level from the chosen template's own `cpu` setting when it names one, such as `x86-64-v4`.
- Template builds and new clusters check the datacenter's `user-tag-access` and `registered-tags` first, as Proxmox VE does.
- Tags match with their case, and `existing` allows tags already on guests the token can audit.
- Only `Sys.Modify` on `/` bypasses the tag options.
- Cluster admission reads Proxmox VE for 2 seconds at most, and each template's configuration once.
- A failed or late read admits the cluster, and the creation hook, which retries, checks its storage and templates.
- A Proxmox VE node that shows online but whose status cannot be read is left out of placement and counted in `NodesSkipped`.

### Fixed

- A cluster storage outside `workload_datastores` no longer leaves the clusters' tokens failing with a permission error.
- A node whose status cannot be read no longer stops every cluster from being placed.
- A cluster whose name matches a Proxmox VE pool holding other VMs is refused instead of taking the pool over.
