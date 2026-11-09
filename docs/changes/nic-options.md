### Added

- A cluster's `network` variable takes an `mtu` (1280 to 65520) and `queues` (1 to 64) for its nodes' interface, set in Proxmox VE and inside the node.
- The wizard and the controller refuse an interface MTU above the bridge's.
