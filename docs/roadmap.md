# Roadmap

What Kubehenge's first release does, what comes next, and what may come later.

## First release

- ClusterClasses for kubeadm, RKE2 and k3s, each passing the end-to-end suite.
- In every cluster: Proxmox CSI plugin, Proxmox cloud controller manager, control-plane virtual IP, CNI (the distribution's own or one chosen per cluster).
- Add-ons: Rancher, with Kubehenge's clusters registered in it, Headlamp, monitoring, pod security through Pod Security Admission, Kyverno or both, CIS hardening, Istio in ambient mode, and Cilium Cluster Mesh, connecting the pod networks of clusters that run Cilium.
- A security baseline from the CIS Kubernetes Benchmark in every cluster.
- The VM templates for every distribution (`kubehenge template`).
- The appliance: one line in the Proxmox VE shell installs it as an LXC container, whose interface creates the management cluster and keeps it upgraded; the same management cluster from one Terraform/OpenTofu apply for those who prefer it.
- The interface: create, list, scale, upgrade and delete clusters, choose the distribution and add-ons, download a kubeconfig, with permissions following Proxmox VE's on each cluster's pool.
- Upgrades without a shell: the appliance upgrades itself from a button, or automatically to new patch or minor releases of Kubehenge, installing only signed releases; each cluster can upgrade Kubernetes automatically inside a maintenance window its owner sets.
- The optional Kubernetes entry inside Proxmox VE's own interface.
- Placement: a cluster's VMs spread over every online node, or over the nodes chosen by hand or from one of Proxmox VE's HA node-affinity rules; the cluster page shows which node each VM runs on and warns when control-plane VMs share one; optionally, Proxmox VE HA keeps the control-plane VMs apart and restarts them after a node failure.

## Next

- A Talos ClusterClass, once Cluster API's Talos providers support ClusterClass in a release.

## Later

Driven by what users ask for. Candidates: clusters across several Proxmox VE nodes and clusters, backup and restore with Proxmox Backup Server, more add-ons, and OKD clusters once Cluster API's OpenShift Assisted providers support ClusterClass and work with the Proxmox VE provider.
