### Internal

- AlmaLinux, Rocky Linux and Leap node images carry a cloud-init fix that dual-stack nodes need to boot. Their build fails unless cloud-init renders a dual-stack network.
- kubeadm node images turn IPv6 forwarding on, which kubeadm's checks require of IPv6 and dual-stack nodes.
- In the IPv6-first end-to-end tests, Proxmox VE's API certificate names the IPv4 address the tests reach it on.
