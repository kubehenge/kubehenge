### Changed

- The installer refuses at once a `--ctid` that another guest has.
- When Proxmox VE's download server drops the pinned container template, the installer takes the Debian 13 one from the signed template index and checks its signature.
- A failed management cluster run lists the pods it waited for and why, such as a node without the memory for one.
- Proxmox VE HA never moves cluster nodes to a host whose CPU is too old for the node OS, such as Rocky Linux 10 without x86-64-v3.

### Fixed

- A management cluster run whose sign-in runs out keeps its state, so the next run no longer fails on pools it had made.
- Stopping the appliance during a management cluster run (migration, stop-mode backup, reboot) gives OpenTofu up to 150 seconds to save its state. The run then fails and says why.
