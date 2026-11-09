### Added

- A cluster's `securityGroups` names Proxmox VE security groups and turns on every node interface's firewall ([clusters.md](clusters.md#firewall)).
- Each VM then accepts the cluster's own traffic, the management cluster's and the appliance's, and the groups accept the rest.
- The wizard asks for the groups in the Network step, and **Options** changes them on a running cluster.
- A change to the security groups replaces every node, one at a time.
- The controller refuses a group that Proxmox VE lacks.
