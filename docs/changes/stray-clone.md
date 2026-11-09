### Fixed

- A machine's VM is cloned once, also when the management cluster's API server is slow, as capmox stores the VM ID first.
- No stopped second clone is left in a cluster's pool, where it kept a deletion from removing the pool.
