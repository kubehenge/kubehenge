### Before you upgrade

- With the Terraform/OpenTofu module, an apply or destroy that removes a management VM fails until you run `qm set <vmid> --protection 0` on it. The appliance lifts it itself.

### Changed

- The management cluster's VMs carry Proxmox VE's protection. A run that replaces or removes one lifts it on that VM alone, and sets it again if the run fails.
