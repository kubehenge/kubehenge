<!-- markdownlint-disable-next-line MD033 -- Markdown can neither size an image nor mark it decorative. -->
# <img src="web/src/logo.svg" alt="" width="40" align="top"> Kubehenge

**Kubernetes for Proxmox VE.**

[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/kubehenge/kubehenge/badge)](https://scorecard.dev/viewer/?uri=github.com/kubehenge/kubehenge)

> [!WARNING]
> **Alpha: do not use it in production.** Any release may change Kubehenge completely, its settings, interface and ClusterClasses included, and nothing is guaranteed yet to keep working from one release to the next.

Kubehenge creates and runs Kubernetes clusters on Proxmox VE. Pick a distribution per cluster (kubeadm, RKE2, k3s or Talos), add Rancher or Headlamp where you want them, and manage it all from a web interface that signs in with your Proxmox VE users, LDAP, Active Directory and OpenID Connect realms included.

VMware ships Kubernetes inside vSphere as the vSphere Kubernetes Service, and OpenStack ships it as Magnum. Both are built on Cluster API. Proxmox VE has no equivalent; Kubehenge is one, built the same way.

## What it does

- **Create, scale, upgrade and delete clusters** on Proxmox VE VMs, declaratively, with nodes replaced one at a time on upgrades and dead nodes replaced automatically.
- **Your choice of distribution,** none locked in: kubeadm (upstream Kubernetes), RKE2, k3s and Talos Linux, each with its own CNI or Cilium or Calico chosen per cluster; Talos nodes are upgraded in place through Talos's own API.
- **Proxmox VE wired in:** persistent volumes through the Proxmox CSI plugin, node lifecycle through the Proxmox cloud controller manager, a virtual IP for the control plane.
- **Clusters kept apart:** each cluster gets its own Proxmox VE pool, user and API token with rights on its own VMs and volumes only, held on its control-plane nodes only.
- **Add-ons per cluster:** Rancher, with every Kubehenge cluster registered in it, and Headlamp; CIS hardening, pod security, monitoring, and backups with Velero to an S3 store, restored from the interface.
- **Sign in to clusters your way:** each cluster can trust your OpenID Connect providers, set once for every cluster or per cluster, with tokens and certificates working beside them; one shared Headlamp in the appliance opens every cluster you may see, and, where the administrator lets it, signs those who may download a cluster's kubeconfig in to it.
- **Upgrades without a shell:** clusters can upgrade Kubernetes on their own inside a maintenance window you set, and Kubehenge upgrades itself from a button, or automatically, installing only signed releases.
- **One appliance:** one line in the Proxmox VE shell installs Kubehenge as a small LXC container; its interface creates the management cluster and looks after it from then on. Infrastructure-as-code users can apply the same module with Terraform/OpenTofu instead.

## Getting started

On a Proxmox VE 9 node, as root:

```sh
curl -fsSLO https://github.com/kubehenge/kubehenge/releases/latest/download/install.sh
sh install.sh
```

The script checks everything it downloads against checksums it carries, creates the appliance container and prints its address. Sign in there with your Proxmox VE account, set up the management cluster on the setup page it shows on first sign-in, then create clusters. Step by step: [docs/install.md](docs/install.md).

## Documentation

| | |
| --- | --- |
| [Installing](docs/install.md) | requirements, the appliance, the management cluster |
| [Clusters](docs/clusters.md) | creating, scaling, upgrading and deleting clusters, add-ons, permissions |
| [Upgrading](docs/upgrading.md) | the appliance, the management cluster and the clusters |
| [Uninstalling](docs/uninstalling.md) | removing clusters, the management cluster and the appliance |
| [Troubleshooting](docs/troubleshooting.md) | logs and the common failures |
| [Architecture](docs/architecture.md) | how it is built, and why |
| [VM templates](docs/templates.md) | the templates clusters are cloned from |
| [Testing](docs/testing.md) | the test tiers and what counts as supported |
| [Roadmap](docs/roadmap.md) | what comes next |
| [Releasing](docs/releasing.md) | how releases are built, signed and checked |
| [Changelog](docs/changelog.md) | what each release changed |

Contributions are welcome: [CONTRIBUTING.md](.github/CONTRIBUTING.md). Security reports go privately through [SECURITY.md](.github/SECURITY.md).

## Licence

[Apache-2.0](LICENSE).

Kubehenge is an independent project and is not affiliated with or endorsed by Proxmox Server Solutions GmbH. Proxmox is a registered trademark of Proxmox Server Solutions GmbH.
