### Changed

- The installer asks the network first: IP version, network and VLAN, then DHCP, router advertisements or a static address per version ([install.md](install.md#install-the-appliance)).
- A static address may be typed without `/prefix`. Its gateway is found from the network and kept in the address's subnet.
- An IPv4-only container takes no IPv6.
- A failed step keeps the installer open. Change the setting at fault, try again, or quit and keep or remove the container.
- A change of network or of the Proxmox VE API after a failure reaches the container that is already there.
- Once a failure screen says the container is kept, only "Quit and remove it" removes it. Ctrl+C and a dropped session keep it.
- Without a terminal, a failed install's container is removed unless `--keep-failed` is given.
- On a terminal the summary is shown unless `--yes` is given.
- The installer saves its answers (never a secret) when the install starts and restores them next time, saying so on screen. A finished install deletes them.
- The header shows the Kubehenge logo, and the screens fit 80 by 24.
- Without a terminal the installer prints the plan, a line as each step starts and the cause beside a failed step.
- `install.sh` downloads the release once and uses the cached copy while it matches its checksum.

### Added

- `--yes`, `--dry-run` and `--keep-failed` ([options](install.md#options)).
- `install.sh --help` lists the options without a node, root or download.
- An install that stops with answers given ends on the command that repeats it with the same answers.
- The installer checks an address for use with its own ARP probe and says whether it found the address free, in use or not checked.

### Fixed

- An address that cannot work is refused with the reason: a network or broadcast address, a gateway outside the subnet, or an IPv6 link-local address.
- A VLAN taken from the node's route is no longer kept when another network is chosen.
- A saved answer that fails the checks is dropped and named on screen. `--ctid` and `--hostname` override saved answers.
- Each run writes its own log, two in the same second included, and the newest ten are kept.
- The connection check tries the Proxmox VE API's port for half a minute after the container starts. A gateway that drops ping or answers late no longer stops a working setup.
