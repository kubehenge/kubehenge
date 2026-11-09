### Added

- After a restore, the Administration page says that automatic upgrades lost their key.
- The installer protects the appliance's container, so Proxmox VE refuses to destroy it or restore over it.
- The docs say how to test a backup without a second appliance on the network.
- An upgrade checks for room first, about three times what is installed, in the updater and in `install.sh --upgrade`.
- The interface warns administrators when the appliance's address no longer matches its certificate, with the command that renews it.
- `install.sh --certificate` points this node's Kubernetes entry at the appliance's new address and prints the command for the other nodes.

### Changed

- Switching automatic upgrades on names the cause when it fails.
- The appliance's updater tries a download up to five times when a proxy or mirror drops the connection or answers 502, 503, 504 or 429.

### Fixed

- An appliance whose certificate has expired upgrades again, where its health check rolled back every upgrade.
- After a restore, switching automatic upgrades off and on works again.
- `install.sh --upgrade --proxmox-url <URL>` moves the appliance to another address of the Proxmox VE API, checked as at install.
- A full disk no longer leaves the old software half replaced.
- A slow link no longer fails an appliance download at 10 minutes, as only two minutes without data end a try.
- An upgrade cut off by a reboot or a kill is undone at the interface's next start, where its status said "under way" for ever.
- `install.sh --upgrade` undoes its own cut-off upgrade when run again.
- After a restore to another ID or a clone, the backup check and the interface's commands find the appliance by its MAC address.
- A guest ID that is gone no longer counts as backed up.

### Security

- The installer sets up Debian's daily security updates in the container, through its proxy.
- `install.sh --upgrade` adds the daily security updates to appliances that lack them.
