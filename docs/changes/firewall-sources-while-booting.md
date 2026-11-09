### Changed

- The setup log says plainly when Proxmox VE refused the management VMs' addresses for lack of `VM.GuestAgent.Audit`, and carries on.

### Fixed

- A node's firewall sources follow the cluster from the VM's first start, not only once the node is ready.
- A node whose cloud-init was slow or failed no longer keeps the firewall sources it was cloned with.
