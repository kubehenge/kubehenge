### Added

- Clusters can be IPv6-only or dual-stack on Talos, kubeadm, RKE2 and k3s with each of their CNIs, the families chosen at creation and the first one primary.
- IPv6 and dual-stack are unsupported until an end-to-end test passes on them ([clusters.md](clusters.md#ipv6-and-dual-stack)).
- An IPv6 cluster gets pod and Service ranges of its own, `fd5e:4b1c:9a20::/56` and `fd5e:4b1c:9a21::/112` unless they clash.
- Load-balancer addresses, BGP routers and DNS servers can be IPv6, and the API address is of the primary family.
- An IPv6-only cluster sets BGP's router ID, as its nodes have no IPv4 address to use.
- The management cluster and the appliance take IPv6 and dual-stack addresses too.
