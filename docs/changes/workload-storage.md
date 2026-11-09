### Added

- The volume owner VM is protected in Proxmox VE and says in its description not to delete it. A cluster whose owner VM disappeared gets a `VolumeOwnerMissing` event with the old VM ID.

### Changed

- The default StorageClass sets `discard` and `ssd`, so thin storage takes back the space of deleted data. The controller recreates a cluster's existing class once to apply it.

### Fixed

- Deleting a cluster looks only at the storages it used, on the nodes where they are active, so offline nodes no longer block it. The stall message says what abandoning leaves behind.
