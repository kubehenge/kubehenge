### Changed

- The management cluster's VMs take random VM IDs from the upper half of Proxmox VE's `next-id` range (Datacenter, Options), else from 10000 to 99998. The setup stops when none is free.
