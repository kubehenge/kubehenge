### Fixed

- A Talos etcd restore starts etcd again when a failed image pull leaves it stopped, instead of timing out with the cluster down. This covers workload clusters and the management cluster.
- A Talos etcd restore waits for the other control-plane nodes to rejoin etcd, and reboots one that a failed image pull left stopped. If it runs out of time, it names the node and Talos's reason.
