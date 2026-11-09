### Added

- Load balancing has a BGP mode, for routed address ranges and switches that drop gratuitous ARP.
- In BGP mode MetalLB peers with your routers from the workers, and kube-vip announces the API address from the control planes.
- BGP mode needs MetalLB and a worker, and is not available on Talos.
- BGP sessions can carry a TCP-MD5 password, stored as the cluster's credential and never in a ClusterClass, chart value or `BGPPeer`.

### Changed

- In BGP mode the add-on room checks count frr-k8s, which has resource requests of its own.
