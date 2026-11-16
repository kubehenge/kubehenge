### Added

- A site **VM ID range** in Setup and Administration, `workload_vm_id_range` in the module and `--vm-id-range` on `kubehenge appliance settings set`. New clusters and VM templates take their IDs from it.
- A cluster's own `vmIDRange`, in the wizard and **Options**. A range too narrow for the nodes and an upgrade is refused, and a full one gives the cluster a `VMIDRangeFull` condition ([clusters.md](clusters.md#variables)).
