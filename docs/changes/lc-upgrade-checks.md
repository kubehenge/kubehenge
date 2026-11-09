### Added

- The upgrade dialog warns of deprecated APIs in use that the target version removes.
- The upgrade dialog warns of fail-closed webhooks with under two ready endpoints or all on one node.
- The health identity may read `/metrics`, webhook configurations and endpoint slices for these checks.
- An automatic upgrade waits, with `AutoUpgradeSkipped` and a warning event, while the checks find something or cannot read the cluster.
- `kubehenge cluster upgrade NAME --version V` prints the same list and asks before it upgrades.
- A deprecated API holds an automatic upgrade only while its request count moved in the last 7 days.
- The controller keeps the request counts in the annotation `kubehenge.dev/deprecated-api-use`.
- **Upgrade anyway** on the Health page lets a skipped automatic upgrade go ahead for one minor version.
- `kubehenge cluster accept-upgrade` and the annotation `kubehenge.dev/acknowledged-upgrade-findings` do the same.
- A cluster on a Kubernetes minor older than any offered for new clusters gets `KubernetesOutOfSupport` and a Health warning.
- The appliance's notifications repeat monthly while a cluster stays out of support.
- `UpgradeFindingsNextMinor` and `UpgradeFindingsNextPatch` hold what an upgrade to the next minor or patch would run into.
- Changing `spec.topology.version` with kubectl or GitOps prints those findings as warnings and is never refused.
- The demo fleet has a cluster out of support and paused, and one whose automatic upgrade waits for a removed API.
- A paused cluster whose certificates run out gets `CertificatesExpiring` with the reason `Paused`.
- A cluster paused for 7 days shows **Paused**, with the date, on its Health page.
