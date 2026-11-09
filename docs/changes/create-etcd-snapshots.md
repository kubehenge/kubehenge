### Added

- The create wizard offers etcd snapshots on its add-ons step, on by default where the class offers a backup target. They share the backups' access key, and a cluster started from another copies them.

### Changed

- Etcd snapshots and backups on one cluster use the same target, as one access key reaches one store, and the webhook refuses two.
- Etcd snapshots start in the browser's time zone, like backups.
