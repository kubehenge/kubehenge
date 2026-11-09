### Changed

- A new address of the nodes, management cluster or appliance reaches every running node's firewall through its `kubehenge` IPSet ([clusters.md](clusters.md#firewall)).
- A change to the `kubehenge` IPSet replaces no node.
- The appliance records its own addresses, so one renewed by DHCP reaches the clusters' firewalls within a minute.

### Removed

- The management cluster module's `appliance_addresses` setting is gone.
