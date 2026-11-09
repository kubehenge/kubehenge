### Added

- While Proxmox VE nodes run different major versions, new VMs go to the oldest version's nodes with room, so a drain or HA move back works. Otherwise they go to newer nodes, with a Warning event.
- The Health page lists the nodes of each Proxmox VE version, as "Proxmox VE 8 and 9 nodes in one cluster". The cluster carries them in the annotation `kubehenge.dev/proxmox-versions`.
- The Health page lists a cluster whose VMs have different interface MTUs on different nodes. The network's `mtu` setting gives every VM the same size ([clusters.md](clusters.md#interface-mtu-and-queues)).
- [install.md](install.md) has an "Upgrading Proxmox VE" section.

### Fixed

- While Proxmox VE refuses HA rules before migrating its HA groups, Kubehenge sets `HARulesPending` and tries again after 5 minutes, where the reconcile failed.
- Kubehenge writes no HA rule while nodes run different major versions, and offers no HA rule placements then.

### Security

- Cluster API's token holds the module's own roles, with only the privileges the provider's calls need ([install.md](install.md#what-cluster-apis-token-may-do)). It loses `VM.Migrate`, `VM.Snapshot` and others that Proxmox VE's roles granted.
