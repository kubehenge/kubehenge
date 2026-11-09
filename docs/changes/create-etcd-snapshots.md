### Before you upgrade

- A cluster whose etcd snapshots and backups use different targets is refused at its next change of either. Point both at one target first.

### Added

- The create wizard offers etcd snapshots on its add-ons step, on by default where the class offers a backup target. A cluster started from another copies its snapshot settings.

### Changed

- A cluster's etcd snapshots and backups use the same target, as they share one access key.
- Etcd snapshot schedules use the browser's time zone, like backups.
