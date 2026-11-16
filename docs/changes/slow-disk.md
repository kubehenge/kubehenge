### Fixed

- The management cluster installs on slow storage, where a disk sync takes seconds. etcd waits 15 s for a request instead of 7 s, and a dead leader is replaced after 5 s ([clusters.md](clusters.md#etcd-storage)).
- An apply that fails on an etcd request timing out runs again by itself.
- A chart whose install failed is installed again by the next apply. Before, the apply stopped on the release name being in use.
- A workload cluster whose Proxmox VE address is a host name comes up. Before, no node was initialised.
