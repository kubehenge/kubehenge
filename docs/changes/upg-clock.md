### Added

- A cluster's network takes up to 8 `ntp` time servers, as **Time servers** in the create wizard, a Cluster or `kubehenge cluster create -f`. Nodes fall back to the hypervisor's clock.
- A kubeadm, RKE2 or k3s node waits up to 3 minutes for chrony before its bootstrap makes certificates. It stops with `clock not synced` when neither a time server nor the host clock works.
- The Health page lists a Proxmox VE node that runs the cluster's VMs and whose clock is over 30 seconds from the controller's. Users with `Sys.Audit` on the node see it.
- A Proxmox VE ticket refused seconds after sign-in reads `Proxmox VE nodes disagree about the time`.
- The expired release index error names the appliance's clock.
- UDP 123 is listed with the nodes' other outbound traffic.

### Changed

- The interface's certificate starts an hour back.

### Fixed

- A Talos node boots after 5 minutes without time sync, where it waited for ever and was replaced again and again. The Health page says `clock not synced`.
