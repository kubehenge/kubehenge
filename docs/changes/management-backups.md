### Added

- The management cluster runs the Proxmox CSI plugin, with the storage class `kubehenge-management` (Retain) on its VMs' storage and a token that deletes no volume.
- A backup volume, the protected disk of the VM `kubehenge-backups`, outlives every management VM. Its claim is `kubehenge-system/kubehenge-backups`.
- The management cluster's etcd is backed up onto that volume every 6 hours, 28 kept, encrypted with the state passphrase, with no settings. A bucket target still gets its own.
- Each snapshot comes with an export of the clusters, their classes, the Secrets Kubehenge owns and the setup settings.
- The appliance keeps an encrypted copy of the latest snapshot and export, up to 1 GiB. The disk warning names the copy near its cap.
- Recovery restores the newest snapshot from the volume, else from the appliance's copy, and its log says which.
- Administration, Health and `kubehenge appliance etcd-backups` show where the backups are and the last one. A failed backup degrades Health.

### Changed

- Removing Kubehenge keeps the backup volume. The Remove dialog can delete it too, off by default.
