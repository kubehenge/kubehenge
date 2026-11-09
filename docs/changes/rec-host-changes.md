### Added

- Machines on a Proxmox VE node that reboots or is in HA maintenance are not replaced for 30 minutes. The Machines and Health views say why, and the Cluster has the condition `RemediationHeld`.
- `hostOutageGraceMinutes` sets that wait, 30 by default, 0 for no ceiling, otherwise 5 to 10080. The wizard, **Options** and `kubehenge cluster create` and `update` (`--host-outage-grace-minutes`) set it.
- A host removed from the Proxmox VE cluster ends the wait at once, since it will not come back.
- The management cluster's Health says when the controller's token lacks `Sys.Audit` on `/`, which hides HA maintenance and removed hosts.
- Storage unavailable on a node running a cluster's VMs shows in Health and holds replacement of the machines on it.

### Changed

- New VMs avoid a Proxmox VE node in HA maintenance. With no node left, the refusal names why.
