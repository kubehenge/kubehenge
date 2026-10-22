# Security

Kubehenge holds Proxmox VE credentials and creates the Kubernetes clusters people run their workloads on, so security reports get priority over everything else.

## Reporting a vulnerability

Report it privately through [GitHub's security advisories](https://github.com/kubehenge/kubehenge/security/advisories/new), never in a public issue, discussion or pull request. If you cannot use GitHub, write to [security@kubehenge.dev](mailto:security@kubehenge.dev). Include what is affected, how to reproduce it, and what an attacker gains.

You will get an answer within a week. We confirm the problem, agree a disclosure date with you (normally within 90 days, sooner for anything actively exploited), fix it in a release, and publish an advisory crediting you unless you would rather not be named.

## Supported versions

Fixes go into the newest minor release. Before 1.0.0, upgrade to it to get them; the appliance can do that by itself (Administration page, automatic upgrades).

## What is in scope

- The appliance: its web interface, its sign-in through Proxmox VE, the updater and the install script.
- Kubehenge's controller and its ClusterClasses, and the credentials and permissions it creates on Proxmox VE.
- The management-cluster module and the release pipeline: signing, provenance and the published artefacts.

Vulnerabilities in Proxmox VE, Kubernetes, Cluster API and its providers, Talos Linux or the add-ons go to those projects; tell us too if Kubehenge's use of them makes them worse.

## Checking a release

Releases are built at SLSA Build Level 3: by a reusable workflow on GitHub-hosted runners, from a signed tag on a signed commit of main that passed every required check. Every file of a release and every image it publishes carries Sigstore-signed build provenance and an SPDX SBOM, the images are signed with Sigstore as well, and the appliance installs only releases listed in an index signed offline with the project's release key, whose public half is built into every release. Published releases are immutable and their tags cannot be moved. How to check a release yourself is in [docs/install.md](../docs/install.md#verifying-a-release), and how it is built in [docs/releasing.md](../docs/releasing.md).
