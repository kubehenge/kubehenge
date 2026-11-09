### Added

- A cluster's network takes up to 8 optional `ntp` time servers, offered as **Time servers** in the create wizard.
- A Cluster or `kubehenge cluster create -f` sets the time servers too.
- Every node syncs to the time servers and falls back to the hypervisor's clock.
- A kubeadm, RKE2 or k3s node waits up to 3 minutes for chrony before its bootstrap makes certificates.
- A node with neither a working time server nor host clock stops with `clock not synced`.
- The Health page lists a Proxmox VE node whose clock is over 30 seconds from the controller's.
- The host clock warning shows to users with `Sys.Audit` on the node, for nodes that run the cluster's VMs.
- A Proxmox VE ticket refused seconds after sign-in reads `Proxmox VE nodes disagree about the time`.
- The expired release index error names the appliance's clock.
- UDP 123 is listed with the nodes' other outbound traffic.

### Changed

- The interface's certificate starts an hour back.

### Fixed

- A Talos node boots after 5 minutes without time sync, where it waited for ever and was replaced again and again.
- The Health page says `clock not synced` for such a Talos node.
