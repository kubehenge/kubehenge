### Added

- Load balancing has a BGP mode for routed ranges and switches that drop gratuitous ARP, with MetalLB peering from the workers and kube-vip announcing the API address. It needs a worker, not Talos.
- BGP sessions can carry a TCP-MD5 password, stored as the cluster's credential and never in a ClusterClass, chart value or `BGPPeer`.

### Changed

- In BGP mode the add-on room checks count frr-k8s, which has resource requests of its own.
