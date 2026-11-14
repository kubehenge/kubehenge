### Fixed

- A dropped SSH session removes the installer's half-made container, and Ctrl+C no longer waits out the two-minute address check. A `nohup` install ignores the drop.
- `curl … | sh` asks its questions on the terminal again, as the shell installer did, instead of running without any.
- The installer prints one line per step when `TERM` is `dumb` or unset.
- A default route over a VLAN device such as `vmbr1.20` puts the container on that bridge and VLAN. A bridge that is not VLAN aware is refused, also when named with `--bridge` and `--vlan` or in the **VLAN** field.
- Open vSwitch bridges and VLAN-aware SDN VNets take the route's VLAN.
- The installer removes only a container it made itself, also after a cancel during the template's extraction. Another installer's container stays.
- A cancel during the last protection step says the appliance exists and how to protect or remove it. Ctrl+C or a dropped session on the end screen just closes it.
- After a failure the installer says what it removed and what stays, such as the downloaded template. Ctrl+C on the failure screen keeps the failure's message.
- A TMPDIR short of room for OpenTofu's download fails early, and a template storage with room is preferred over one without.
- A node with no bridge stops the installer at once, and a missing DHCP address names `--vlan` again.
- An API that serves only its own certificate gets a warning, as a renewal breaks the appliance.
- `apt` in the container waits 20 seconds instead of 120 for each try at an unreachable mirror. It keeps its 3 retries, so such a mirror costs about 90 seconds per index file.
