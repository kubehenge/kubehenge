### Changed

- The create wizard asks Proxmox VE for the user's privileges once, so 20 nodes and 30 VNets take 31 calls at most, not 600.

### Fixed

- A machine still finds its VM when Proxmox VE lists the VM without a name, as when its node drops out of the statistics. One with no VM shows "Not reporting".
