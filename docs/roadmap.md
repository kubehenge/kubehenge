# Roadmap

What each release changed is in the [changelog](changelog.md).

## Now

### Clusters

- ClusterClasses for kubeadm, RKE2, k3s and Talos, each passing the end-to-end suite. Talos nodes are upgraded in place through Talos's own API, one at a time, control planes first.
- In every cluster:
  - Proxmox CSI plugin
  - Proxmox cloud controller manager
  - control-plane virtual IP
  - CNI (the distribution's own or one chosen per cluster)
  - Traefik as the ingress controller on every node, and as the Gateway API's Gateway
  - optionally, addresses for Services of type LoadBalancer with MetalLB or kube-vip
- A security baseline from the CIS Kubernetes Benchmark in every cluster.

#### Nodes and scaling

- VM templates for every distribution. kubeadm, RKE2 and k3s get a node OS each cluster chooses (AlmaLinux 10 or Rocky Linux 10, in image mode, SELinux enforcing).
- Node OS templates rebuilt weekly with their updates. Nodes move to each build in their maintenance windows once it is a day old.
- The cluster autoscaler for worker pools, from none up to 100 workers.
- Placement over every online node, chosen nodes or one of Proxmox VE's HA node-affinity rules. Optionally, Proxmox VE HA keeps the control-plane VMs apart and restarts them after a node failure.

### Add-ons

- Rancher, with Kubehenge's clusters registered in it.
- Headlamp, monitoring, and logs with Loki and Grafana Alloy.
- Pod security through Pod Security Admission, Kyverno or both.
- CIS hardening.
- Istio in ambient mode and Cilium Cluster Mesh.
- Certificates from cert-manager (by ACME or from a CA of the cluster's own).
- GitOps with Argo CD or Flux.
- Backups with Velero and encrypted etcd snapshots, both to S3 stores and restored from the interface.

### Appliance and interface

- The appliance, an LXC container installed by one line in the Proxmox VE shell, whose interface creates the management cluster and keeps it upgraded. One Terraform/OpenTofu apply creates the same management cluster.
- The interface creates, lists, scales, upgrades and deletes clusters, with changes staged and applied together after a plan. It picks each cluster's distribution and add-ons and downloads its kubeconfig.
- Permissions follow Proxmox VE's on each cluster's pool. Optionally, the shared Headlamp opens each cluster signed in as cluster-admin for those who may download its kubeconfig.
- The appliance upgrades itself and, by choice, the management cluster, without a shell. Each cluster can upgrade Kubernetes automatically inside a maintenance window its owner sets.
- The optional Kubernetes entry inside Proxmox VE's own interface.

## Next

- The controller manager's and scheduler's metrics on Talos, from the first Talos release that gives them serving certificates of the cluster CA.
- openSUSE Leap 16.1 Immutable as a node OS, once openSUSE releases it.
- The Proxmox VE provider (capmox) from one of its own releases, once one carries the fixes. Until then Kubehenge builds it from a commit of its main branch.

## Later

Driven by what users ask for, such as clusters across several Proxmox VE clusters and more add-ons. OKD clusters may follow once Cluster API's OpenShift Assisted providers support ClusterClass and work with the Proxmox VE provider.
