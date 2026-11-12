### Changed

- The installer is a full-screen guide on a terminal (`kubehenge install`, started by `install.sh`). It asks DHCP or static, then only what is ambiguous, and shows a summary before it changes anything.
- The installer's steps show as a live list, and their commands' output goes to `/var/log/kubehenge-install.log`.
- `install.sh` only downloads and checks the release. Without a terminal, a node whose own network is static needs `--ip` (`--ip dhcp` to keep the old default).

### Fixed

- A container that gets no DHCP address is no longer deleted without a hint. The guide offers a static address and retries, and an unattended run names `--ip <address>/<prefix> --gateway <address>`.
- A static address that already answers on the network is refused before anything is created.
