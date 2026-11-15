### Changed

- etcd snapshots are on by default for every distribution. Without a backup target they go to the management cluster's backup volume, encrypted, with the cluster's schedule and keep.
- A bucket target keeps the snapshots alone. The volume keeps no second copy, so its room stays with clusters that have no bucket.
- A snapshot that would leave less than a tenth of the volume free is refused, with the condition reason `VolumeFull`.
- On a node's own storage the controller asks for the volume, and the appliance wakes it with its own wake. The condition reads `WaitingForVolume` until then.
- Talos clusters restore from the volume through the interface, kubectl and the CLI. kubeadm, RKE2 and k3s download the snapshot and restore by hand.
- The distributions' own upload needs a bucket target.
