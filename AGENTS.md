# Kubehenge

Kubernetes for Proxmox VE: Cluster API underneath, kubeadm, RKE2, k3s or Talos per cluster, optional add-ons. It installs as a small LXC appliance. Its web interface signs in with Proxmox VE users and creates and looks after a Talos management cluster.

The engineering rules are in `.github/CONTRIBUTING.md` and apply in full. Read it first, then `docs/architecture.md` and `docs/testing.md`.

## Layout

| Path | What |
| --- | --- |
| `cmd/kubehenge`, `pkg/` | the `kubehenge` binary: controller, web interface backend, appliance updater, CLI |
| `web/` | the interface's React/TypeScript frontend, embedded in the binary |
| `charts/kubehenge` | the ClusterClasses and the controller |
| `management-cluster/` | Terraform/OpenTofu module for the Talos management cluster |
| `appliance/` | the LXC appliance's install script and units |
| `appliance/pve-entry/` | the opt-in Proxmox VE interface entry |
| `test/integration/`, `test/e2e/` | integration assets and the API server harness with Cluster API's webhooks. End-to-end tests against a Proxmox VE host |
| `scripts/` | the pinned tools (`tools.sh`). The image builds of capmox and the Add-on Provider for Helm (`provider-image.sh`) and the fixes they carry (`capmox-patches/`, `helm-addon-patches/`). The check of the Talos providers from Kubehenge's forks (`verify-providers.sh`) |
| `docs/` | user guides, architecture, templates, testing, releasing, roadmap, changelog |
| `website/` | kubehenge.dev: `docs/` with a home page, built with VitePress and published from main |

## Checks

`make test` (lint and unit) before every change is handed back: Go under the race detector, the frontend, the charts, the module and the shell scripts. The end-to-end targets (`make e2e-*`, `docs/testing.md`) need a Proxmox VE host and run in CI.
