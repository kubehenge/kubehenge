---
layout: home
hero:
  name: Kubehenge
  text: Kubernetes for Proxmox VE
  tagline: Create and run Kubernetes clusters on Proxmox VE from a web interface that signs in with your Proxmox VE users, built on Cluster API.
  image:
    src: /logo.svg
    alt: ""
  actions:
    - theme: brand
      text: Get started
      link: /docs/install
    - theme: alt
      text: GitHub
      link: https://github.com/kubehenge/kubehenge
features:
  - title: Clusters, declaratively
    details: Create, scale, upgrade and delete clusters on Proxmox VE VMs, with nodes replaced one at a time on upgrades and dead nodes replaced automatically.
  - title: Your choice of distribution
    details: kubeadm, RKE2 and k3s, each with its own CNI or Cilium or Calico chosen per cluster; Talos to follow.
  - title: Proxmox VE wired in
    details: Persistent volumes through the Proxmox CSI plugin, node lifecycle through the Proxmox cloud controller manager, a virtual IP for the control plane.
  - title: Clusters kept apart
    details: Each cluster gets its own Proxmox VE pool, user and API token, with rights on its own VMs and volumes only.
  - title: Add-ons per cluster
    details: Rancher, with every Kubehenge cluster registered in it, and Headlamp.
  - title: One appliance
    details: One line in the Proxmox VE shell installs a small LXC container whose interface creates the management cluster and looks after it, upgrading itself from signed releases.
---

<!-- markdownlint-disable-next-line MD041 -- The home layout's hero, set above, is the page's heading. -->
::: warning Alpha
Do not use it in production. Any release may change Kubehenge completely, its settings, interface and ClusterClasses included.
:::
