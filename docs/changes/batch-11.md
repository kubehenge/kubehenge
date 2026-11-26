### Changed

- The option to use a cluster's own cert-manager is gone, and a cluster that set `useExisting` has it taken off.
- `kubehenge appliance health` shows each stage, the time left and a verdict. `--quiet` keeps it silent and `--json` prints one object.

### Fixed

- Persistent volumes work on shared storage such as LVM on iSCSI. A Pending add-on claim shows under `AddonsWaiting` as `<add-on> has no volume: <reason>`.
- A cert-manager installed by hand under the release name `cert-manager` is never taken over. A cluster being deleted while it sets `useExisting` finishes deleting.
- Monitoring, logs and Rancher install once cert-manager is deployed. `AddonsWaiting` and the Health page name what they wait for, and a cluster's own cert-manager that does not work is a conflict.
- A working cert-manager the cluster already runs is used, however it was installed. Kubehenge installs its own only once a full read of the cluster found none, and never over another.
- Release pins on the VM templates page work on an appliance: the interface's account may read and update the pins, and the other pages it could not read now answer too.
- Setup refuses a Site CA that lacks the root a host's chain ends at, also when the appliance itself trusts that root. It names the root with its fingerprint and offers to add it.
- Setup names a certificate that is not for servers at once, and no longer offers to add a public root that never fixes it.
- After a restart the appliance's interface answers at once, without waiting for the container to send traffic first.
- `pct exec <ctid> -- kubehenge` finds the command.
- Template builds retry a dropped connection to Sigstore instead of failing.
- A cluster can no longer be given the management cluster's, its API's or the appliance's address, and a machine whose VM lacks its address after boot says so.
- The refusal of a management node's address names the node. Existing installs need the management cluster's module applied again.
- Each of the appliance's addresses is refused on the bridge and VLAN of its own network card. Existing appliances learn theirs at the next `install.sh --upgrade` or once an administrator signs in.
- Without `managementEndpoint`, a new cluster or a move of its autoscaler onto the control planes is refused and not offered. A cluster already there keeps its other changes.
- Refusing a change of address families, bridge or VLAN says why: the API address and the pods' ranges were made for that network.
- An etcd limit can be lowered while every member's database stays under 60% of the new one, as read from every member in the last 18 minutes. Before, it could only rise.

### Internal

- End-to-end tests run on the newest Proxmox VE release.
