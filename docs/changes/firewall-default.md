### Changed

- New clusters get the Proxmox VE firewall on every node. It opens the API, ingress, load balancer, NodePorts and ping, and Talos's API on Talos.
- SSH stays closed unless a security group lets it in.
- The `firewall` variable opens further ports with `ports`, and `enabled: false` turns the firewall off.
- `servingPorts: false` closes the serving ports, leaving the cluster's own traffic, the security groups and the further ports.
- The wizard and the cluster's options offer it as **Serving ports open**.
- Security groups now apply before the serving ports, so a group can drop or narrow them.
- The lockout check refuses only security groups, never the firewall itself.
- With Proxmox VE's datacenter firewall off, a cluster's firewall does nothing. The cluster then gets the `FirewallInactive` condition.
- The wizard says so on its firewall field and on Confirm, and still creates the cluster.
- The cluster's options and `kubehenge cluster list` show it too, and a warning event is recorded.
- A cluster made before this gets the firewall at its next change, which replaces every node once. Kubehenge is in alpha and migrates nothing.
