### Fixed

- A VM renamed in Proxmox VE is renamed back within 15 seconds with a `VMRenamedBack` event, so Kubernetes no longer deletes its node as gone when it is next not ready.
- A worker moved to another Proxmox VE node gets its `topology.kubernetes.io/zone` label updated and its CSI plugin pod restarted, and new node-local volumes are created on the right host.
