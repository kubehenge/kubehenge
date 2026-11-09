### Changed

- The setup log says plainly when Proxmox VE refused the management VMs' addresses for lack of `VM.GuestAgent.Audit`, and carries on.

### Fixed

- A node's firewall sources follow the cluster from the VM's first start, so a node with slow or failed cloud-init no longer keeps the sources it was cloned with.
