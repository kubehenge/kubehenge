### Changed

- The installer asks the network first: IP version, network and VLAN, then DHCP, router advertisements or a static address per version ([install.md](install.md#install-the-appliance)).
- A static address may be typed without `/prefix`. Its gateway is found from the network and kept in the address's subnet.
- An IPv4-only container takes no IPv6.
- A step that fails keeps the installer open, with the choice to change the setting at fault, try again, or quit keeping or removing the container.
- A change of network after a failure reaches the container that is already there.
- The installer saves its answers (never a secret) when the install starts and restores them next time, saying so on screen. A finished install deletes them.
- The terminal header is the trilithon, and the screens fit 80 by 24.
- Without a terminal the installer prints the plan, a line as each step starts and the cause beside a failed step.
- `install.sh` downloads the release once and uses the cached copy while it matches its checksum.

### Added

- `--yes`, `--dry-run` and `--keep-failed` ([options](install.md#options)).
- `install.sh --help` lists the options from any shell, with no node and no root.
- The installer checks an address for use with its own ARP probe and says whether it found the address free, in use or not checked.

### Fixed

- An address that cannot work is refused with the reason: a network or broadcast address, a gateway outside the subnet, or an IPv6 link-local address.
- The route's VLAN no longer follows the user onto another network.
- The connection check tries the Proxmox VE API's port for half a minute after the container starts. A gateway that drops ping or answers late no longer stops a working setup.
