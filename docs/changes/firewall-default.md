### Before you upgrade

- Existing clusters get the firewall at their next change, which replaces every node once. SSH to the nodes then needs a security group ([clusters.md](clusters.md#ssh-to-the-nodes)), or set `firewall.enabled: false` first.

### Changed

- New clusters get the Proxmox VE firewall on every node. It opens the API, ingress, load balancer, NodePorts and ping, and Talos's API on Talos.
- SSH stays closed unless a security group lets it in.
- The `firewall` variable opens further ports with `ports`, and `enabled: false` turns the firewall off.
- `servingPorts: false`, or clearing **Serving ports open** in the wizard or the cluster's options, closes the serving ports. The cluster's own traffic, the security groups and the further ports stay.
- Security groups now apply before the serving ports, so a group can drop or narrow them.
- The lockout check refuses only security groups, never the firewall itself.
- With Proxmox VE's datacenter firewall off, a cluster's firewall does nothing, and the cluster gets the `FirewallInactive` condition.
- The wizard, the cluster's options, `kubehenge cluster list` and a warning event report an inactive datacenter firewall.
