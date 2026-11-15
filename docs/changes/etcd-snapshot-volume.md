### Changed

- etcd snapshots are on by default for every distribution. Without a backup target they go to the management cluster's backup volume, encrypted, with the cluster's schedule and keep.
- A bucket target keeps the snapshots alone. The volume keeps no second copy, so its room stays with clusters that have no bucket.
- A snapshot that would leave less than a tenth of the volume free is refused, with the condition reason `VolumeFull`.
- On a node's own storage the controller asks for the volume, and the appliance wakes it with its own wake. The condition reads `WaitingForVolume` until then.
- Every distribution restores from the volume, also after etcd lost its quorum. kubeadm, RKE2 and k3s bring the first node up alone and stream the snapshot to it.
- `kubehenge cluster etcd-snapshots` lists, takes, downloads and restores a cluster's snapshots. `kubehenge cluster removed-etcd-snapshots` lists and deletes removed clusters' snapshots.
- The controller's `--csi-proxy-image` flag is now `--image`.
- A removed cluster keeps its volume snapshots unless the remove dialog, `--delete-etcd-snapshots` or `kubehenge.dev/delete-etcd-snapshots: "true"` asks.
- Without a bucket target the wizard and the Options dialog offer **Backup volume** beside the class's buckets.
- The distributions' own upload needs a bucket target.
