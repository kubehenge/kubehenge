### Changed

- `kubehenge appliance health` shows each stage, the time left and a verdict. `--quiet` keeps it silent and `--json` prints one object.

### Fixed

- Monitoring, logs and Rancher install only once cert-manager is deployed, so their add-ons no longer fail on a missing Certificate kind.
- Release pins on the VM templates page work on an appliance: the interface's account may read and update the pins, and the other pages it could not read now answer too.
- Setup refuses a Site CA that lacks the root a host's chain ends at, also when the appliance itself trusts that root. It names the root with its fingerprint and offers to add it.
- Setup names a certificate that is not for servers at once, and no longer offers to add a public root that never fixes it.
- After a restart the appliance's interface answers at once, without waiting for the container to send traffic first.
- `pct exec <ctid> -- kubehenge` finds the command.
- Template builds retry a dropped connection to Sigstore instead of failing.
- A cluster can no longer be given the management cluster's, its API's or the appliance's address, and a machine whose VM lacks its address after boot says so.
- The refusal of a management node's address names the node. Existing installs need the management cluster's module applied again.
- The appliance's address is refused on its own bridge and VLAN. Existing appliances need `install.sh --upgrade` to record their network.

### Internal

- End-to-end tests run on the newest Proxmox VE release.