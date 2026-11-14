### Added

- The management cluster runs the Proxmox CSI plugin, with the storage class `kubehenge-management` (Retain) on its VMs' storage and a token that deletes no volume.
- A backup volume, the protected disk of the VM `kubehenge-backups`, outlives every management VM. Its claim is `kubehenge-system/kubehenge-backups`.
- The management cluster's etcd is backed up onto that volume every 6 hours, 28 kept, encrypted with the state passphrase, with no settings. A bucket target still gets its own.
- The appliance keeps an encrypted copy of the latest snapshot, up to 1 GiB.

### Changed

- Removing Kubehenge in the appliance keeps the backup volume unless everything is asked to go.
