### Changed

- On node-local storage, Kubehenge keeps a worker on every node that holds the cluster's volumes. A replacement or a growing pool's new worker goes to such a node first.
- The `VolumesWithoutWorker` condition names the volumes whose node has no worker, what keeps the cluster's VMs off that node, and the fix.
- The create wizard and the change checks warn when a cluster has fewer workers than nodes its volumes are on.
