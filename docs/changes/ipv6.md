### Added

- Clusters can be IPv6-only or dual-stack, on Talos, kubeadm, RKE2 and k3s with each of their CNIs.
- A cluster's address families are chosen when it is created, the first one primary.
- IPv6 and dual-stack are unsupported until an end-to-end test passes on them ([clusters.md](clusters.md#ipv6-and-dual-stack)).
- An IPv6 cluster gets pod and Service ranges of its own, `fd5e:4b1c:9a20::/56` and `fd5e:4b1c:9a21::/112` unless they clash.
- Every node gives its kubelet its address of each family.
- Load-balancer addresses, BGP routers and DNS servers can be IPv6, and the API address is of the primary family.
- BGP has a router ID setting, which an IPv6-only cluster needs, as each node otherwise gives its own IPv4 address.
- The management cluster and the appliance take IPv6 and dual-stack addresses too.
