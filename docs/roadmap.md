# Roadmap

What Kubehenge does now, what comes next, and what may come later. What each release changed is in the [changelog](changelog.md).

## Now

- ClusterClasses for kubeadm, RKE2, k3s and Talos, each passing the end-to-end suite; Talos nodes upgraded in place through Talos's own API, one at a time, control planes first.
- In every cluster: Proxmox CSI plugin, Proxmox cloud controller manager, control-plane virtual IP, CNI (the distribution's own or one chosen per cluster), Traefik as the ingress controller on every node, the Gateway API with Traefik's Gateway, and optionally addresses for Services of type LoadBalancer with MetalLB or kube-vip.
- Add-ons: Rancher, with Kubehenge's clusters registered in it, Headlamp, monitoring, logs with Loki and Grafana Alloy, pod security through Pod Security Admission, Kyverno or both, CIS hardening, Istio in ambient mode, Cilium Cluster Mesh, certificates from cert-manager (by ACME or from a CA of the cluster's own), GitOps with Argo CD or Flux, and backups with Velero to S3 stores, restored from the interface.
- etcd snapshots to the same S3 stores, encrypted, and restored from the interface.
- A security baseline from the CIS Kubernetes Benchmark in every cluster.
- The VM templates for every distribution, on a node OS each cluster chooses (AlmaLinux 10 or Rocky Linux 10, in image mode, SELinux enforcing), built again every week with their updates, and every cluster's nodes moved to each build inside their maintenance windows once it has soaked for a day.
- The appliance: one line in the Proxmox VE shell installs it as an LXC container, whose interface creates the management cluster and keeps it upgraded; the same management cluster from one Terraform/OpenTofu apply for those who prefer it.
- The interface: create, list, scale, upgrade and delete clusters, with changes staged and applied together after a plan; choose the distribution and add-ons, download a kubeconfig, with permissions following Proxmox VE's on each cluster's pool; optionally, the shared Headlamp opens each cluster signed in as cluster-admin for those who may download its kubeconfig.
- The cluster autoscaler for worker pools, from none up to 100 workers.
- Upgrades without a shell: the appliance upgrades itself and, by choice, the management cluster; each cluster can upgrade Kubernetes automatically inside a maintenance window its owner sets.
- Placement over every online node, chosen nodes or one of Proxmox VE's HA node-affinity rules; optionally, Proxmox VE HA keeps the control-plane VMs apart and restarts them after a node failure.
- The optional Kubernetes entry inside Proxmox VE's own interface.

## Next

- The controller manager's and scheduler's metrics on Talos, once a Talos release gives them serving certificates of the cluster CA; Kubehenge scrapes them from the first release that does.
- openSUSE Leap 16.1 Immutable as a node OS, once openSUSE releases it.
- The Proxmox VE provider (capmox) from one of its own releases, once one carries the fixes for which Kubehenge builds it from a commit of its main branch.

## Later

Driven by what users ask for. Candidates: clusters across several Proxmox VE clusters, restoring a backup into another cluster from the interface, more add-ons, and OKD clusters once Cluster API's OpenShift Assisted providers support ClusterClass and work with the Proxmox VE provider.
