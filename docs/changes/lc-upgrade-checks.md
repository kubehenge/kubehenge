### Added

- The upgrade dialog warns of deprecated APIs in use that the target version removes and of fail-closed webhooks with under two ready endpoints or all on one node.
- An automatic upgrade waits, with `AutoUpgradeSkipped` and a warning event, while the checks find something or cannot read the cluster.
- `kubehenge cluster upgrade NAME --version V` prints the same list and asks before it upgrades.
- A deprecated API holds an automatic upgrade only while its request count moved in the last 7 days, kept in the annotation `kubehenge.dev/deprecated-api-use`.
- **Upgrade anyway** on the Health page, `kubehenge cluster accept-upgrade` or the annotation `kubehenge.dev/acknowledged-upgrade-findings` lets a skipped automatic upgrade go ahead for one minor version.
- A cluster on a Kubernetes minor older than any offered for new clusters gets `KubernetesOutOfSupport` and a Health warning, and the appliance's notifications repeat monthly.
- `UpgradeFindingsNextMinor` and `UpgradeFindingsNextPatch` hold what the next minor or patch upgrade would hit. Changing `spec.topology.version` in kubectl or GitOps prints them as warnings, never refusing.
- The demo fleet has a cluster out of support and paused, and one whose automatic upgrade waits for a removed API.
- A paused cluster whose certificates run out gets `CertificatesExpiring` with the reason `Paused`, and one paused for 7 days shows **Paused**, with the date, on its Health page.

### Changed

- The health identity may read `/metrics`, webhook configurations and endpoint slices for the upgrade checks.
