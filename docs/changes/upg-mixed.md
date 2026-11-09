### Added

- The controller reads each Proxmox VE node's version every minute.
- While nodes run different major versions, new VMs go to the oldest version's nodes with room, so a drain or HA move back works.
- When none of those has room, new VMs go to newer nodes, and a Warning event on the cluster says so.
- A machine is never refused for the Proxmox VE versions alone.
- The Health page lists the nodes of each Proxmox VE version, as "Proxmox VE 8 and 9 nodes in one cluster".
- The cluster carries the versions in the annotation `kubehenge.dev/proxmox-versions`.
- The Health page lists a cluster whose VMs have different interface MTUs on different nodes.
- The network's `mtu` setting gives every VM the same size, as on Proxmox VE 9 an unset MTU follows the bridge ([clusters.md](clusters.md#interface-mtu-and-queues)).
- [install.md](install.md) has an "Upgrading Proxmox VE" section.

### Fixed

- Kubehenge sets `HARulesPending` and tries again after 5 minutes while Proxmox VE refuses HA rules before migrating its HA groups.
- The reconcile no longer fails while Proxmox VE refuses HA rules.
- Kubehenge writes no HA rule while nodes run different major versions, and offers no HA rule placements then.

### Security

- Cluster API's token holds the module's own roles in place of `PVEVMAdmin`, `PVETemplateUser`, `PVEDatastoreAdmin` and `PVESDNUser`.
- The roles list only the privileges the provider's calls need ([install.md](install.md#what-cluster-apis-token-may-do)).
- The token no longer holds `VM.Clone` on the workload pool, `VM.Migrate`, `VM.Snapshot` or the guest agent file privileges.
