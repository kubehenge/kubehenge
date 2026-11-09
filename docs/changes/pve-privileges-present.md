### Changed

- A refusal from Proxmox VE reads "Proxmox VE refused:" with its own reason, such as "VM is locked (backup)".
- Only a lost connection, a timeout or a gateway error reads "did not answer".
- A refused second factor says the factor may be locked, or for a recovery key that the cluster may lack quorum.
- Such refusals still count against the sign-in limit, as Proxmox VE answers them like a wrong code.
- The docs' example roles for cluster users hold `Pool.Audit`.
- A user without `Pool.Audit` is told so on the cluster page and on reboot, instead of "The machine has no VM in the cluster's pool".

### Fixed

- Privileges granted through a pool, or by an ACL without propagate, count as held.
- Storages, VNets and guests reached that way no longer show as out of reach in the interface.
- A sign-in name over 64 characters, realm included, is refused at once with the limit, not with an error that read like an outage.
