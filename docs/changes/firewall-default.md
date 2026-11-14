### Changed

- New clusters get the Proxmox VE firewall on every node. It opens the API, ingress, load balancer, NodePorts and ping.
- The `firewall` variable opens further ports with `ports`, and `enabled: false` turns the firewall off.
- With Proxmox VE's datacenter firewall off, a cluster's firewall does nothing. The cluster then gets the `FirewallInactive` condition.
- A cluster made before this gets the firewall at its next change, which replaces every node once.
