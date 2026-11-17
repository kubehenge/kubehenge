### Fixed

- A Talos etcd restore starts etcd again when a failed image pull leaves it stopped, instead of timing out with the cluster down. This covers workload clusters and the management cluster.
- A Talos etcd restore waits until the other control-plane nodes have joined the restored etcd. It reboots a node whose etcd a failed image pull left stopped. A restore that runs out of time names the node, with Talos's reason.
