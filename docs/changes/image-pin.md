### Before you upgrade

- Template builds now also reach `vault.almalinux.org`. Allow it where a firewall or proxy limits what the appliance, the management cluster and the Proxmox VE nodes reach.

### Fixed

- Template builds no longer fail with `is not listed in its checksum file` once AlmaLinux or Rocky Linux publishes a new minor release. The image then comes from the publisher's vault ([templates.md](templates.md#image-checks)).

### Security

- Each node OS's cloud image is pinned by its SHA-256. A build refuses an image whose signed checksum differs from the pin.
