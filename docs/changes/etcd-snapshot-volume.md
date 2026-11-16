### Added

- `kubehenge cluster etcd-snapshots` lists, takes, downloads and restores a cluster's snapshots. `kubehenge cluster removed-etcd-snapshots` lists and deletes deleted clusters' snapshots.

### Changed

- etcd snapshots are on by default for every distribution, also for clusters that never set them. Without a backup target they go to the management cluster's backup volume, encrypted.
- A cluster with a bucket target keeps its snapshots there only.
- A snapshot that would leave less than a tenth of the volume free is refused, with the condition reason `VolumeFull`.
- On node-local storage the backup volume attaches only for a backup. A snapshot waits for it, up to a minute, with the condition reason `WaitingForVolume`.
- Every distribution restores from the volume, also after etcd lost its quorum.
- A deleted cluster keeps its etcd snapshots on the backup volume. The Delete cluster dialog, `--delete-etcd-snapshots` or the annotation `kubehenge.dev/delete-etcd-snapshots: "true"` deletes them too.
- The wizard and the etcd snapshots dialog offer **Backup volume** as a target, beside the bucket targets.
- The distributions' own upload needs a bucket target.

### Internal

- The controller's `--csi-proxy-image` flag is now `--image`.
