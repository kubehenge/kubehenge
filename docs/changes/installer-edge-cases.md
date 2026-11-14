### Fixed

- A dropped SSH session removes the installer's half-made container, and Ctrl+C no longer waits out the two-minute address check.
- `curl … | sh` asks its questions on the terminal again, as the shell installer did, instead of running without any.
- The installer prints one line per step when `TERM` is `dumb` or unset.
- A default route over a VLAN device such as `vmbr1.20` puts the container on that bridge and VLAN.
- The installer checks for 1 GiB free in its work directory and skips template storages with no room.
- A node with no bridge stops the installer at once, and a missing DHCP address names `--vlan` again.
- An API that serves only its own certificate gets a warning, as a renewal breaks the appliance.
- `apt` in the container gives up on an unreachable mirror after 20 seconds.
