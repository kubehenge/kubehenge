### Added

- The installer takes `--vlan` and `--mtu` for the container's network.
- The installer's `--tofu <file>` installs OpenTofu from a local archive, checked against the script's checksum.
- Once the container has an address, the installer checks its network from inside it.
- The network check tests the gateway, the name server and the API's port in turn.
- A failed network check names the option to check (`--vlan`, `--gateway`, `--dns` or the node's firewall).
- SDN VNets work wherever a bridge is chosen, listed with their zone and alias.
- `workload_bridges` takes `zone/vnet`, with `PVESDNUser` granted in that zone.
- A "Time servers" setting writes Talos's `TimeSyncConfig` for the management nodes, and empty keeps Talos's default servers.
- The setup checks the settings against Proxmox VE before it runs.
- The setup's checks cover storages, bridges, memory, disk space, answering addresses and leftover pools, roles, users and groups.
- A failed setup check names the node or storage and the fix.
- The setup page shows the checks before Apply and disables Apply while an error stands.
- Too little free space is an error on thick LVM and ZFS without sparse, a warning on LVM-thin, Ceph RBD, directories and the rest.
- The setup warns of VMs sharing nodes and of an API address that is one node's.
- The setup warns of pod or service ranges that overlap a node's networks, and shows the ranges chosen.
- The management cluster's health shows when the controller cannot reach the Proxmox VE API from inside the cluster.
- A Talos node drained before its upgrade reboot records the event `TalosDrained` on its Cluster.

### Changed

- The installer checks `--bridge` before it downloads anything, and takes a bridge, Open vSwitch bridge or SDN VNet.
- Without `--bridge`, the installer uses the bridge of the node's default route, else `vmbr0`.
- `--ip` and `--gateway` take IPv4 only and are checked at once.
- The installer ranks storages by type, local block storage, then directories, NFS, CIFS and CephFS, each by free space.
- The installer leaves out storages with under 9 GiB free and prints the type of the one it picks.
- The installer stops when the cluster has an appliance already.
- `--upgrade` and `--certificate` on the wrong node name the node that runs the container.
- The installer stops early, in plain words, without quorum, with only loopback name servers or with too little room for downloads.
- A failed download gives curl's exit code and the HTTP status.
- An option without its value reports `<option> needs a value`.
- A storage name is matched as written.
- A container that cannot resolve the API's name makes the installer try the next name or address the certificate covers.
- The installer names every API address it tried when none works.
- The usage text and the docs describe how `--template-storage` is chosen.
- A refused or timed-out connection to the Proxmox VE API is reported as a failed connection, not an untrusted certificate.
- A container that gets no DHCP address names `--vlan` and the DHCP server.
- A VLAN tag on a VNet of a VLAN zone is refused.
- New management VMs get their MAC addresses from Proxmox VE, so two installs on one network never share them.
- An existing management cluster keeps its MAC addresses and is not changed by an upgrade.
- A cut-off apply sends OpenTofu an interrupt and waits up to five minutes for it to write its state before killing it.
- A failed run's error carries OpenTofu's error diagnostics, not only the exit status.
- Every Helm release of the management module that waits has a timeout of 900 seconds.
- Changing the management cluster's Proxmox VE nodes once it is Ready is refused, as its VMs would migrate.
- An answer from Proxmox VE larger than 1 MiB fails with an error naming the path.
- Kyverno's two admission controllers run on different nodes, or one on a control-plane node when there is one worker.

### Fixed

- The installer asks about the Proxmox VE interface entry on the same terminal as its other questions, also when piped into `sh`.
- A dropped session or an interrupt removes the installer's half-made container and temporary files.
- Cluster nodes start again when their Proxmox VE host reboots, as templates set `onboot` for their clones.
- Nodes cloned from an older template get `onboot` when a rolling upgrade replaces them.
- AlmaLinux and Rocky Linux templates get the kernel's extra modules with the kernel itself.
- A bake no longer fails in the hours after a kernel release on a mirror without that kernel's modules.
- Draining a node no longer waits on Kyverno's admission controllers.
