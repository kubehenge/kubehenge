### Changed

- The create form asks Proxmox VE for the user's privileges once, so 20 nodes and 30 VNets take 31 calls at most, not 600.

### Fixed

- A VM that Proxmox VE lists without a name, as its node dropped out of the statistics, is matched to its machine by guest ID.
- A machine with no VM to match shows "Not reporting" as its node.
