### Fixed

- The setup check refuses a Proxmox VE URL name that none of the cluster's DNS servers resolves, and warns when they and the appliance disagree on it.
- The setup check warns when the cloud-init images are on node-local storage while the disks are on shared storage and VMs can move between nodes.
