### Added

- The create wizard offers etcd snapshots on its add-ons step, on by default wherever the class offers a backup target.
- Etcd snapshots share the backups' access key, entered once.
- A new cluster started from another takes its etcd snapshot settings.

### Changed

- Etcd snapshots and backups on one cluster use the same target, as one access key reaches one store, and the webhook refuses two.
- Etcd snapshots start in the browser's time zone, like backups.
