# Contributing to Kubehenge

Kubehenge creates, scales, upgrades and deletes Kubernetes clusters on Proxmox VE, with a distribution and add-ons chosen per cluster. Its web interface signs in with Proxmox VE users. It does for Proxmox VE what the vSphere Kubernetes Service does for vSphere and Magnum for OpenStack.

## Where things are

| Path | What |
| --- | --- |
| `README.md` | what Kubehenge is, for users |
| `docs/` | user guides (installing, clusters, upgrading, uninstalling, troubleshooting), the design (`architecture.md`, `templates.md`, `roadmap.md`, `releasing.md`, `testing.md`) and the changelog |
| `website/` | kubehenge.dev: the pages of `docs/` with a home page, built by `make website` (VitePress) and published from main |
| `management-cluster/` | the Terraform/OpenTofu configuration of the management cluster |
| `charts/kubehenge/` | the Helm chart the management cluster installs: Kubehenge's ClusterClasses and its controller |

### Code and tooling

| Path | What |
| --- | --- |
| `cmd/kubehenge`, `pkg/` | the `kubehenge` command and the Go code it shares with the interface (`pkg/ui`) |
| `web/` | the interface's frontend, React and TypeScript, built by `make web` into `pkg/ui/dist` and embedded in the binary |
| `appliance/` | the appliance's install script and service units. `appliance/pve-entry/` is the optional entry in Proxmox VE's own interface |
| `test/integration/`, `test/e2e/` | the integration tier's assets and the API server it runs, with Cluster API's webhooks where a test asks. The end-to-end tests run against a Proxmox VE host |

### Scripts and CI

| Path | What |
| --- | --- |
| `scripts/` | the pinned tools `make test` needs, Go and Node among them (`tools.sh`), and the Python linters' lock file (`lint/`). The image builds of capmox and the Add-on Provider for Helm (`provider-image.sh`) and the fixes they carry (`capmox-patches/`, `helm-addon-patches/`). The check of the Talos providers from Kubehenge's forks (`verify-providers.sh`) |
| `.github/` | CI on every pull request, fuzzing, the release workflows, issue templates, this guide and the security policy (`SECURITY.md`) |

## Working on it

`scripts/tools.sh <dir>` installs the pinned tools, Go, Node and uv among them. With them on PATH, `make test` runs every lint and unit test, as CI does on each pull request. `make fmt` formats what the checks would refuse. `make build` builds the `kubehenge` binary with the interface embedded.

## Design

- **Cluster API underneath**, as in the vSphere Kubernetes Service and Magnum:
  - `capmox` for Proxmox VE VMs
  - the upstream bootstrap and control-plane providers for kubeadm, RKE2 and k3s
  - Kubehenge's own forks of them for Talos
  - one tested ClusterClass per distribution
- **The management cluster is an appliance.** It is Talos, and it runs Cluster API and Kubehenge and nothing else.

### Installed and chosen

- **Always installed in every cluster:**
  - the Proxmox CSI plugin and cloud controller manager
  - a control-plane virtual IP
  - a CNI (the distribution's own unless another is chosen)
  - Traefik and the Gateway API
- **Add-ons chosen per cluster**, most as Helm charts through the Cluster API Add-on Provider for Helm:
  - Headlamp, monitoring, logs, backups, etcd snapshots and pod security
  - CIS hardening, Istio, Cilium Cluster Mesh, a load balancer, certificates and GitOps
  - Rancher (with Kubehenge's clusters registered in it)

### Interface and first run

- **Interface:** a separate web app installed onto Proxmox VE as one appliance. It signs in through the Proxmox VE API.
  - Go for the backend and the `kubehenge` command-line tool, React and TypeScript for the frontend, built into one binary.
  - An optional entry in Proxmox VE's own interface leads to it. The installer adds it only when the administrator says yes.
  - The app never depends on that entry.
- **First run:** one Terraform/OpenTofu apply, or the appliance itself, stands up the management cluster.

Details and reasons: `docs/architecture.md`.

## Rules

- **Licence:** Apache-2.0 for everything, the optional Proxmox VE interface entry included.
  - The entry copies none of Proxmox VE's code. Apache-2.0 is compatible with Proxmox VE's AGPL-3.0.
  - Contributions are accepted under the same licence.
  - A new dependency carries one of the licences in `.github/dependency-review-config.yml`. Pull requests are checked against that list together with known vulnerabilities.
- **Proxmox naming:** "Proxmox" never appears in a product, component or domain name, and the Proxmox logo is never used. Refer to "Proxmox VE" by its full name, and never imply that Proxmox Server Solutions endorses this project.

### Support and upstream

- **Supported means tested.**
  - A ClusterClass or add-on counts as supported only once end-to-end tests in CI have run on Proxmox VE. They must have created, scaled, upgraded and deleted a cluster with it (`docs/testing.md`).
- **Upstream first.** Use the maintained upstream Cluster API providers, charts and images.
  - Kubehenge assembles and tests them rather than forking them.
  - Fixes that belong upstream go upstream.

### Forks and own builds

- Talos's bootstrap and control-plane providers come from Kubehenge's own forks.
- Sidero Labs' releases are on a Cluster API contract the core has removed. They have no `TalosControlPlaneTemplate`, which a ClusterClass needs.
- `make verify-providers` checks the forks' releases.
- Until upstream releases carry their fixes, `capmox` and the Add-on Provider for Helm also run Kubehenge's own builds (`scripts/capmox-patches/`, `scripts/helm-addon-patches/`).
- Details in `docs/architecture.md`.

### Versions

- Providers, charts and tooling track their latest release.
  - A pin below latest carries the reason beside it.
  - Renovate (`renovate.json`) proposes every update in one pull request each week.
  - A version it cannot find on its own gets a `# renovate: datasource=… depName=…` comment on the line above. A download's SHA-256 goes on the line below its tag, so the two move together.

### Digests

- Everything Kubehenge pulls or deploys (images, charts, manifests, OS images, downloads, tools) is pinned by its digest as well as its version. Renovate moves the two together.
- Where a consumer takes only a version, Kubehenge gives it the digest beside the version. It makes the consumer pull by the digest or check it.
- Where the publisher's bytes change under a version, its signature is required instead. The few left with neither are named in `docs/architecture.md`.
- An image that another release names for itself has no Renovate comment. Examples are Rancher's webhook, Fleet, shell and Turtles, and the FRR of MetalLB's chart.
- Such an image moves only with that release, as Renovate's pull request for it says.

### Release pins

- RKE2's and k3s's releases are pinned in `pkg/provisioner/releasepins.txt`. Renovate raises the newest release named in its header.
- `make release-pins` (with `GH_TOKEN` set) on that pull request adds every release published since. A unit test requires this.

### Checks and logs

- **Strictest checks.**
  - Every linter, formatter and type checker runs at its strictest setting.
  - A rule is switched off only where it contradicts another or the language's own conventions.
  - A single finding is suppressed only where the rule is wrong for that line. Either way the reason stands beside it.
- **Logs escape what they carry.**
  - Go code logs through `log/slog` and the controller through controller-runtime's zap in JSON. Both escape a value, so nothing a user sends can forge a log line.
  - The linters refuse the `log` package and zap's console output. CodeQL's log injection query is off on that account (`.github/codeql-config.yml`).

### Releases

- Each release publishes the appliance archive and `index.json`. The index lists every release's archive URL and SHA-256, expires two weeks later and is signed with Kubehenge's release key.
- The website publishes the latest index signed again every week.
- The public keys are the lines of `pkg/release/release.pub`, built into every release.
- Appliances install nothing one of them has not signed, never from an expired index and never an older release.
- The release workflow builds everything in a reusable workflow (`release-build.yml`, SLSA Build Level 3).
- It attests build provenance and SPDX SBOMs for every file and image. It signs the images with Sigstore and the index with the release key, then publishes the release once everything verifies.
- The steps are in `docs/releasing.md`.

### Charts and wording

- **Chart versions.** A change to a chart in this repository raises its `version` by the next release.
  - The management cluster's Terraform/OpenTofu configuration installs the charts from their directories and passes each a digest of its files (`chartDigest`).
  - The Helm provider upgrades a release only when its chart version or values change. The digest makes it upgrade a chart changed without a new version.
- **Wording.**
  - British English.
  - Write "Terraform/OpenTofu", never either alone.
