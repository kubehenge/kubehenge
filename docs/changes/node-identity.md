### Added

- A VM renamed in Proxmox VE is renamed back to its machine's name within 15 seconds, with a `VMRenamedBack` event.

### Fixed

- Kubernetes no longer deletes the node of a renamed VM as gone when it is next not ready.
- A worker moved to another Proxmox VE node gets its `topology.kubernetes.io/zone` label updated, and its CSI plugin pod restarts.
- New node-local volumes of a moved worker are created on the right host.
