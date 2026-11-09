### Added

- A cluster whose volume owner VM disappeared gets a `VolumeOwnerMissing` event with the old VMID.
- A stalled cluster deletion says what abandoning it leaves behind.

### Changed

- The default StorageClass sets `discard` and `ssd`, so thin storage takes back the space of deleted data.
- The controller recreates a cluster's existing class once, as Kubernetes does not allow changing its parameters.
- The volume owner VM is protected in Proxmox VE and says in its description not to delete it.
- Deleting a cluster looks only at the storages it used, on the nodes where they are active.

### Fixed

- Offline nodes and storages the cluster never used no longer block its deletion.
