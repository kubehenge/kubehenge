### Added

- A cluster's network takes up to 8 `ntp` time servers, as **Time servers** in the create wizard, a Cluster or `kubehenge cluster create -f`. Nodes fall back to the hypervisor's clock.
- A kubeadm, RKE2 or k3s node waits up to 3 minutes for chrony before its bootstrap makes certificates. It stops with `clock not synced` when neither a time server nor the host clock works.
- The Health page lists a Proxmox VE node that runs the cluster's VMs and whose clock is over 30 seconds from the controller's. Users with `Sys.Audit` on the node see it.
- A Proxmox VE ticket refused seconds after sign-in reads `Proxmox VE nodes disagree about the time`.
- An expired release index error says to check the appliance's clock.
- The docs list UDP 123 among the nodes' outbound traffic.

### Changed

- The interface's certificate is valid from an hour before it is issued, so a browser whose clock runs behind accepts it.

### Fixed

- A Talos node boots after 5 minutes without time sync, where it waited for ever and was replaced again and again. The Health page says `clock not synced`.
