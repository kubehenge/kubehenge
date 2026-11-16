### Added

- The management cluster's etcd is backed up every 6 hours onto a backup volume, with no settings. It keeps 28 snapshots, encrypted with the state passphrase.
- The backup volume is the protected disk of the VM `kubehenge-backups` and outlives every management VM. Its claim is `kubehenge-system/kubehenge-backups`.
- A bucket target keeps its own schedule and retention.
- Each snapshot comes with an export of the clusters, their classes, the Secrets Kubehenge owns and the setup settings.
- On node-local storage the backup volume attaches only during a backup, so the management VMs can migrate.
- On node-local storage, or with a single Proxmox VE node, losing that node can lose the backup volume. Setup, Administration and Health then suggest a bucket target.
- The appliance keeps an encrypted copy of the latest snapshot, export and key, up to 1 GiB in all. The disk warning names the copy near its cap.
- **Recover…** on Administration, Backups and `kubehenge appliance etcd-backups recover` rebuild the management cluster after its VMs are lost.
- A recovery restores whichever is newer, the volume's newest snapshot or the appliance's copy.
- Administration, Health and `kubehenge appliance etcd-backups` show where the backups are and the last one. A failed backup degrades Health.
- Removing Kubehenge keeps the backup volume. The Remove dialog can delete it too, off by default, with the appliance's copy.
- The management cluster runs the Proxmox CSI plugin, with the storage class `kubehenge-management` (Retain) on its VMs' storage and a token that deletes no volume.
