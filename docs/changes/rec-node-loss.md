### Added

- A machine deleting for 20 minutes gets the condition `MachineDeletionStalled`, with why.
- A stuck machine can be forgotten from the interface, `kubehenge cluster machine forget` or the annotation `kubehenge.dev/forget-machine`. That is refused while its VM runs on a node that answers.
- The controller tests every token it relies on every 10 minutes, privileges included. A failure shows in Health, goes out as a notification and gives the cluster `ProxmoxAccessRefused`.

### Fixed

- Machines being deleted on a Proxmox VE node that left the cluster for good no longer stay in Deleting. Kubehenge lets go of them, and the Cluster gets the event `NodeGone`.
- An etcd restore goes on without control-plane VMs that no longer exist in Proxmox VE. With none left, it says to rebuild with `kubehenge cluster create --from <file>`.
