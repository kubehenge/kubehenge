### Changed

- The message "the cluster changed meanwhile" means another user changed the cluster.

### Fixed

- Changes, plans, worker deletions and etcd requests no longer fail with "the cluster changed meanwhile" while the cluster is busy.
