### Added

- The installer protects the appliance's container, so Proxmox VE refuses to destroy it or restore over it, and the docs say how to test a backup without a second appliance.
- The interface warns administrators when the appliance's address no longer matches its certificate, with the command that renews it.
- `install.sh --certificate` points this node's Kubernetes entry at the appliance's new address and prints the command for the other nodes.

### Changed

- Switching automatic upgrades on names the cause when it fails.
- An appliance upgrade outlasts a proxy or mirror that drops a download or answers 502, 503, 504 or 429. A slow link no longer fails it at 10 minutes, only two minutes without data.

### Fixed

- An appliance whose certificate has expired upgrades again, where its health check rolled back every upgrade.
- After a restore, the Administration page says that automatic upgrades lost their key, and switching them off and on works again.
- `install.sh --upgrade --proxmox-url <URL>` moves the appliance to another address of the Proxmox VE API, checked as at install.
- An upgrade checks for room first, about three times what is installed, in the updater and `install.sh --upgrade`, so a full disk no longer leaves the old software half replaced.
- An upgrade cut off by a reboot or a kill is undone at the interface's next start, or when `install.sh --upgrade` runs again, where its status said "under way" for ever.
- After a restore to another ID or a clone, the backup check and the interface's commands find the appliance by its MAC address. A guest ID that is gone no longer counts as backed up.

### Security

- The installer sets up Debian's daily security updates in the container, through its proxy, and `install.sh --upgrade` adds them to appliances that lack them.
