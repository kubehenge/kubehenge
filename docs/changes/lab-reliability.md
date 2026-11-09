### Changed

- The installer refuses a `--ctid` that another guest has at once.
- Once Proxmox's mirror drops the pinned container template, the installer takes the Debian 13 template from the signed template index and checks its signature as `pveam` does.
- A failed management cluster run lists the pods it waited for and why, such as a node without the memory for one.
- High availability never moves cluster nodes to a host whose CPU is too old for the node OS, such as Rocky Linux 10 without x86-64-v3.

### Fixed

- An install cut off while `pct create` runs removes the half-made container and the disks it had allocated.
- A management cluster run cut off at the end of its sign-in keeps its state, as OpenTofu is interrupted five minutes before, and the next run no longer fails on pools it had made.
- Stopping the appliance in a management cluster run (migration, stop-mode backup, reboot) lets OpenTofu write its state, then fails the run saying so. The appliance waits up to 150 seconds.
