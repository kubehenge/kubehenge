### Internal

- AlmaLinux, Rocky Linux and Leap node images carry a cloud-init fix that dual-stack nodes need to boot. Their build fails unless cloud-init renders a dual-stack network.
- kubeadm node images turn IPv6 forwarding on, which kubeadm's checks require of IPv6 and dual-stack nodes.
- In the IPv6-first end-to-end tests, Proxmox VE's API certificate names the IPv4 address the tests reach it on.
- In the IPv6-first end-to-end tests, the management cluster is dual-stack, so that Cluster API reaches the cluster's IPv6 API address.
- On kubeadm clusters whose first address family is IPv6, the API server advertises the node's IPv6 address, which an IPv6 primary Service range requires.
