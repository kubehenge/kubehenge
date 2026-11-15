### Added

- A **Site CA** in Setup and Administration, and `siteCA` in the chart values. Nodes, the release mirror, backup targets and identity providers trust it unless they name their own.
- Saving a new site CA says how many clusters replace their nodes. At most four roll at once, each inside its upgrade window where it has one.
- A paused cluster takes a new site CA once unpaused, and the release mirror's authority reaches nodes with the site CA their cluster was given.
- An expired or damaged site CA is reported on each cluster and stops no controller. A damaged one changes no cluster's site CA until it is fixed.

### Changed

- A release mirror certificate that is damaged or expired is reported on each cluster and no longer stops the controller.
- A certificate bundle refuses a damaged BEGIN or END line, naming its line. Names, comments and openssl's text between the blocks are accepted, as in curl's `cacert.pem`.
- The interface accepts and refuses the same certificate bundles as the server.
- A cluster's certificate bundles are stored as the certificates alone, also from kubectl and GitOps. Keep that form in Git for Argo CD ([clusters.md](clusters.md#authorities)).
- The proxy and chart mirror authorities follow the same layout rule, up to 256 KiB, and are stored as the certificates alone.

### Fixed

- A cluster replaced with a manifest that names no pod or service ranges keeps its own. Before, a k3s cluster's nodes were replaced and other distributions' new nodes took their defaults.
