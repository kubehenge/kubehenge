### Fixed

- The management cluster installs on storage that holds etcd's syncs for seconds. etcd waits 15 s for a request instead of 7 s, and a dead leader is replaced after 5 s ([clusters.md](clusters.md#etcd-storage)).
- An apply that fails on etcd timing out a request runs again, up to three times in all, 30 s apart.
- A chart whose install failed is installed again by the next apply. Before, the apply stopped on the release name being in use.
- A workload cluster whose Proxmox VE address is a host name comes up. Before, the cloud controller manager looked the name up through CoreDNS, which waits for it, so no node was initialised.
