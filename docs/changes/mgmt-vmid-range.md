### Changed

- The management cluster's VMs take random VM IDs from the upper half of Proxmox VE's `next-id` range (Datacenter, Options), and the setup stops if none are free. Without `next-id` they keep 10000 to 99998.
