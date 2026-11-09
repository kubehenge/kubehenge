### Added

- The installer takes `--vlan` and `--mtu` for the container's network, and `--tofu <file>` to install OpenTofu from a local archive, checked against the script's checksum.
- The installer tests the gateway, the name server and the API's port from inside the container. A failure names the option to check (`--vlan`, `--gateway`, `--dns` or the node's firewall).
- SDN VNets work wherever a bridge is chosen, listed with their zone and alias. `workload_bridges` takes `zone/vnet`, with `PVESDNUser` granted in that zone.
- A "Time servers" setting writes Talos's `TimeSyncConfig` for the management nodes. Empty keeps Talos's default servers.
- The setup checks its storages, bridges, memory, disk space, addresses and leftover objects against Proxmox VE before it runs. A failure names the node or storage and the fix, and holds Apply.
- The setup warns of VMs sharing nodes, of an API address that is one node's, and of pod or service ranges that overlap a node's networks. It shows the ranges chosen.
- The management cluster's health shows when the controller cannot reach the Proxmox VE API from inside the cluster.
- A Talos node drained before its upgrade reboot records the event `TalosDrained` on its Cluster.

### Changed

- The installer checks `--bridge` before it downloads anything, and takes a bridge, Open vSwitch bridge or SDN VNet. Without it, the installer uses the bridge of the node's default route, else `vmbr0`.
- `--ip` and `--gateway` take IPv4 only and are checked at once.
- The installer ranks storages: local block storage, then directories, NFS, CIFS and CephFS, each by free space. It leaves out storages with under 9 GiB free.
- The installer stops when the cluster has an appliance already. `--upgrade` and `--certificate` on the wrong node name the node that runs the container.
- The installer stops early, in plain words, without quorum, with only loopback name servers or with too little room for downloads.
- A failed download gives curl's exit code and the HTTP status.
- An option without its value reports `<option> needs a value`. A storage name is matched as written.
- A container that cannot resolve the API's name makes the installer try the other names and addresses the certificate covers. When none works, it names each one it tried.
- The usage text and the docs describe how `--template-storage` is chosen.
- A refused or timed-out connection to the Proxmox VE API is reported as a failed connection, not an untrusted certificate.
- A container that gets no DHCP address names `--vlan` and the DHCP server.
- A VLAN tag on a VNet of a VLAN zone is refused.
- New management VMs get their MAC addresses from Proxmox VE, so two installs on one network never share them.
- A cut-off apply lets OpenTofu write its state before it stops. A failed run's error carries OpenTofu's diagnostics, not only the exit status.
- Changing the management cluster's Proxmox VE nodes once it is Ready is refused, as its VMs would migrate.
- An answer from Proxmox VE larger than 1 MiB fails with an error naming the path.
- Too little free space is an error on thick LVM and ZFS without sparse, and a warning on other storage.

### Fixed

- The installer asks about the Proxmox VE interface entry on the same terminal as its other questions, also when piped into `sh`.
- A dropped session or an interrupt removes the installer's half-made container and temporary files.
- Cluster nodes start again when their Proxmox VE host reboots, as templates set `onboot` for their clones. Older nodes get it when a rolling upgrade replaces them.
- AlmaLinux and Rocky Linux template builds no longer fail in the hours after a kernel release, as the kernel's extra modules come with the kernel itself.
- Draining a node no longer waits on Kyverno's admission controllers, which run on different nodes.
