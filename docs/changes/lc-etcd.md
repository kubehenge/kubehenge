### Added

- Every cluster gets an etcd database limit (`etcd` variable, `quotaGiB` 2 to 8, default 8) and periodic compaction, so etcd no longer refuses writes at the old 2 GiB default.
- The controller reads each etcd member's database size every ten minutes for the cluster page, and Health warns above 60 percent of the limit or on a `NOSPACE` alarm.
- The controller defragments etcd members one at a time, followers first, inside the upgrade window. `kubehenge cluster etcd defrag` and `kubehenge cluster etcd set --quota-gib` act at once.
- A Talos upgrade waits before a control-plane node while another etcd member is unhealthy or a learner, with a message on the cluster and a `TalosUpgradeWaiting` event.
