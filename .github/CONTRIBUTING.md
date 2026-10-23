# Contributing to Kubehenge

Kubehenge creates, scales, upgrades and removes Kubernetes clusters on Proxmox VE, with the distribution of your choice and optional add-ons, from a web interface that signs in with your Proxmox users. It is the piece VMware ships as vSphere Kubernetes Service and OpenStack as Magnum, and which Proxmox VE does not have.

## Where things are

| Path | What |
| --- | --- |
| `README.md` | what Kubehenge is, for users |
| `docs/` | user guides (installing, clusters, upgrading, uninstalling, troubleshooting), the design (architecture, templates, testing, roadmap, releasing) and the changelog |
| `website/` | kubehenge.dev: the pages of `docs/` with a home page, built by `make website` (VitePress) and published from main |
| `management-cluster/` | the Terraform/OpenTofu configuration of the management cluster |
| `charts/kubehenge/` | the Helm chart the management cluster installs: Kubehenge's ClusterClasses |
| `cmd/kubehenge`, `pkg/` | the `kubehenge` command and the Go code it shares with the interface (`pkg/ui`) |
| `web/` | the interface's frontend, React and TypeScript, built by `make web` into `pkg/ui/dist` and embedded in the binary |
| `appliance/` | the appliance's install script and service units; `appliance/pve-entry/`, the optional entry in Proxmox VE's own interface |
| `test/integration/`, `test/e2e/` | the integration tier's assets; the end-to-end tests, run against a Proxmox VE host |
| `scripts/` | the pinned tools `make test` needs, Go and Node among them (`tools.sh`), and the Python linters' lock file (`lint/`); the image builds of capmox, the Add-on Provider for Helm and the Talos control-plane provider (`provider-image.sh`) and the fixes they carry (`capmox-patches/`, `helm-addon-patches/`, `talos-control-plane-patches/`) |
| `.github/` | CI on every pull request, fuzzing, the release workflows, issue templates, this guide and the security policy (`SECURITY.md`) |

## Working on it

`scripts/tools.sh <dir>` installs the pinned tools, Go, Node and uv among them; with them on PATH, `make test` runs every lint and unit test, as CI does on each pull request, and `make fmt` formats what the checks would refuse. `make build` builds the `kubehenge` binary with the interface embedded.

## Design

- **Cluster API underneath**, the design VKS and Magnum use: `capmox` for Proxmox VE VMs, the upstream bootstrap and control-plane providers for kubeadm, Talos, RKE2 and k3s, one tested ClusterClass per distribution.
- **The management cluster is an appliance:** Talos, running Cluster API and Kubehenge and nothing else.
- **Always installed in every cluster:** the Proxmox CSI plugin and cloud controller manager, a control-plane virtual IP, a CNI (the distribution's own unless another is chosen).
- **Add-ons chosen per cluster**, as Helm charts through the Cluster API Add-on Provider for Helm: Rancher (with Kubehenge's clusters registered in it), Headlamp, CIS hardening, pod security, monitoring.
- **Interface:** a separate web app installed onto Proxmox VE as one appliance, signing in through the Proxmox VE API. Go for the backend and the `kubehenge` command-line tool, React and TypeScript for the frontend, built into one binary. An optional entry inside Proxmox VE's own interface, added only when the administrator says yes at install, leads to it; the app never depends on it.
- **First run:** one Terraform/OpenTofu apply, or the appliance itself, stands up the management cluster.

Details and reasons: `docs/architecture.md`.

## Rules

- **Licence:** Apache-2.0 for everything, the optional Proxmox VE interface entry included: it copies none of Proxmox VE's code, and Apache-2.0 is compatible with Proxmox VE's AGPL-3.0. Contributions are accepted under the same licence. A new dependency carries one of the licences in `.github/dependency-review-config.yml`, which pull requests are checked against together with known vulnerabilities.
- **Proxmox naming:** "Proxmox" never appears in a product, component or domain name, and the Proxmox logo is never used. Refer to "Proxmox VE" by its full name, and never imply that Proxmox Server Solutions endorses this project.
- **Supported means tested.** A ClusterClass or add-on counts as supported only once end-to-end tests in CI have created, scaled, upgraded and deleted a cluster with it on Proxmox VE (`docs/testing.md`).
- **Upstream first.** Use the maintained upstream Cluster API providers, charts and images; Kubehenge assembles and tests them rather than forking them. Fixes that belong upstream go upstream.
- **Newest versions.** Providers, charts and tooling track their latest release; a pin below latest carries the reason beside it. Renovate (`renovate.json`) proposes every update in one pull request each week. A version it cannot find on its own gets a `# renovate: datasource=… depName=…` comment on the line above, and a download's SHA-256 goes on the line below its tag, so the two move together.
- **Pinned by digest.** Everything Kubehenge pulls or deploys (images, charts, manifests, OS images, downloads, tools) is pinned by its digest as well as its version, and Renovate moves the two together. Where a consumer takes only a version, Kubehenge gives it the digest beside the version and makes it pull by the digest or check it. Where the publisher's bytes change under a version, its signature is required instead; the few left with neither are named in `docs/architecture.md`. An image that another release names for itself (Rancher's webhook, Fleet, shell and Turtles; the FRR of MetalLB's chart) has no Renovate comment and moves only with that release, as Renovate's pull request for it says.
- **Strictest checks.** Every linter, formatter and type checker runs at its strictest setting. A rule is switched off only where it contradicts another or the language's own conventions, and a single finding is suppressed only where the rule is wrong for that line; either way the reason stands beside it.
- **Logs escape what they carry.** Go code logs through `log/slog` and the controller through controller-runtime's zap in JSON, both of which escape a value, so nothing a user sends can forge a log line; the linters refuse the `log` package and zap's console output, and CodeQL's log injection query is off on that account (`.github/codeql-config.yml`).
- **Releases are signed.** Each release publishes the appliance archive and `index.json`, listing every release's archive URL and SHA-256 and signed with Kubehenge's release key. The public key is `pkg/release/release.pub`, built into every release; appliances install nothing the key has not signed, and never an older release. The release workflow builds everything in a reusable workflow (`release-build.yml`, SLSA Build Level 3), attests build provenance and SPDX SBOMs for every file and image, signs the images with Sigstore and the index with the release key, and publishes the release once everything verifies; the steps are in `docs/releasing.md`.
- **Chart versions:** a change to a chart in this repository raises its `version` by the next release. The management cluster's Terraform/OpenTofu configuration installs the charts from their directories and passes each a digest of its files (`chartDigest`), since the Helm provider upgrades a release only when its chart version or values change; a chart changed without a new version is still upgraded.
- **Wording:** British English; write "Terraform/OpenTofu", never either alone.
