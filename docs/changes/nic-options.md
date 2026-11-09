### Added

- A cluster's `network` variable takes an `mtu` (1280 to 65520) and `queues` (1 to 64) for its nodes' interface, and the MTU reaches the Proxmox VE interface and the node's own.
- An interface MTU above the bridge's is refused, in the wizard and by the controller, where Proxmox VE lists the bridge's MTU.
