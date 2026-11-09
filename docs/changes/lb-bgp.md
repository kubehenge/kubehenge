### Added

- Load balancing has a BGP mode for routed ranges and switches that drop gratuitous ARP, which needs a worker. kube-vip announces the API address over BGP too, except on Talos.
- BGP sessions can carry a TCP-MD5 password, stored as the cluster's credential and never in a ClusterClass, chart value or `BGPPeer`.
- In BGP mode the add-on room checks count frr-k8s.
