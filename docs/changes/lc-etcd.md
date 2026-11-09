### Added

- Every cluster gets an etcd database limit (`etcd` variable, `quotaGiB` 2 to 8, default 8) and periodic compaction.
- The controller reads each etcd member's database size every ten minutes, and the cluster page shows it.
- The Health view warns above 60 percent of the etcd limit or on a `NOSPACE` alarm.
- The controller defragments etcd members one at a time, followers first, inside the upgrade window.
- `kubehenge cluster etcd defrag` and `kubehenge cluster etcd set --quota-gib` act at once.
- A Talos upgrade waits before a control-plane node while another etcd member is unhealthy or a learner.
- A waiting Talos upgrade shows a message on the cluster and a `TalosUpgradeWaiting` event.

### Fixed

- Etcd no longer refuses writes at 2 GiB, the default that kubeadm, RKE2, k3s and Talos ran.
