### Fixed

- Clusters are created again on Proxmox VE with libpve-access-control 9.2.1 or newer. Each cluster's user creates its own token, as Proxmox VE now refuses the controller a token for another user.
- A cluster's VM that an administrator took out of Proxmox VE HA by hand no longer stops the cluster's HA updates.
