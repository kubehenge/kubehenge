### Fixed

- A dropped SSH session removes the installer's half-made container, and Ctrl+C no longer waits out the two-minute address check. A `nohup` install ignores the drop.
- `curl … | sh` asks its questions on the terminal again, as the shell installer did, instead of running without any.
- The installer prints one line per step when `TERM` is `dumb` or unset.
- A default route over a VLAN device such as `vmbr1.20` puts the container on that bridge and VLAN. The installer refuses when that bridge is not VLAN aware, and names both fixes.
- The installer removes only a container it made itself. A cancel during the last protection step says the appliance exists and how to protect or remove it.
- A TMPDIR short of room for OpenTofu's download fails early, and a template storage with room is preferred over one without.
- A node with no bridge stops the installer at once, and a missing DHCP address names `--vlan` again.
- An API that serves only its own certificate gets a warning, as a renewal breaks the appliance.
- `apt` in the container gives up on an unreachable mirror after 20 seconds and keeps its own retries.
