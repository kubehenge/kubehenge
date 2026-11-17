### Fixed

- A Talos etcd restore starts etcd again when a failed image pull leaves it stopped, instead of timing out with the cluster down. This covers workload clusters and the management cluster.
