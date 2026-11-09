### Added

- Kubehenge lets go of machines on a node that left the cluster for good, and the Cluster gets the event `NodeGone`.
- A node that is only offline keeps its machines.
- A machine deleting for 20 minutes gets the condition `MachineDeletionStalled`, with why.
- A stuck machine can be forgotten from the interface or with `kubehenge cluster machine forget`.
- `kubectl annotate machine MACHINE kubehenge.dev/forget-machine=MACHINE` forgets a stuck machine too.
- Forgetting is refused, in every path alike, while the VM runs on a node that answers.
- A forgotten machine leaves the ProxmoxCluster's node locations.
- The controller tests every token it relies on every 10 minutes.
- A refused token, a certificate from another CA or a missing privilege shows in Health and goes out as a notification.
- A cluster whose token fails gets the condition `ProxmoxAccessRefused`.
- The token test checks the privileges the module's roles grant, and fails when they differ.

### Fixed

- Machines being deleted on a Proxmox VE node that left the cluster for good no longer stay in Deleting.
- An etcd restore goes on without control-plane VMs that no longer exist in Proxmox VE.
- With no control-plane VM left, the restore says to rebuild with `kubehenge cluster create --from <file>`.
