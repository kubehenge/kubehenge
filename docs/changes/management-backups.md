### Added

- The management cluster runs the Proxmox CSI plugin, with the storage class `kubehenge-management` (Retain) on its VMs' storage and a token that deletes no volume.
- A backup volume, the protected disk of the VM `kubehenge-backups`, outlives every management VM. Its claim is `kubehenge-system/kubehenge-backups`.
- On a node's own storage the volume attaches only during a backup, so the management VMs can migrate. Before each backup the appliance corrects the zone label of a node whose VM moved.
- Where one Proxmox VE host's loss can take the backup volume (a node's own storage, or a cluster of one node), Setup, Administration and Health suggest a bucket target until one is set.
- The management cluster's etcd is backed up onto that volume every 6 hours, 28 kept, encrypted with the state passphrase, with no settings. A bucket target keeps its own schedule and retention.
- Each snapshot comes with an export of the clusters, their classes, the Secrets Kubehenge owns and the setup settings.
- The appliance keeps an encrypted copy of the latest snapshot, export and key, up to 1 GiB in all. The disk warning names the copy near its cap.
- **Recover…** on Administration, Backups and `kubehenge appliance etcd-backups recover` restore the newest snapshot of the volume and the appliance's copy. They wait for the volume first, and the log says which was read and why.
- Administration, Health and `kubehenge appliance etcd-backups` show where the backups are and the last one. A failed backup degrades Health.

### Changed

- Removing Kubehenge keeps the backup volume. The Remove dialog can delete it too, off by default, with the appliance's copy.
