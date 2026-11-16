### Before you upgrade

- A cluster's certificate bundles are stored as the certificates alone. Keep that form in Git, or Argo CD shows the cluster as out of sync ([clusters.md](clusters.md#authorities)).

### Added

- A **Site CA** in Setup and Administration, and `siteCA` in the chart values. Nodes, the release mirror, backup targets and identity providers trust it unless they name their own.
- Saving a new site CA says how many clusters replace their nodes. At most four roll at once, each inside its upgrade window where it has one.
- A paused cluster takes a new site CA once unpaused.
- An expired or damaged site CA is reported on each cluster and stops no controller. A damaged one changes no cluster's site CA until it is fixed.

### Changed

- A release mirror certificate that is damaged or expired is reported on each cluster and no longer stops the controller.
- A certificate bundle with a damaged BEGIN or END line is refused, with the line's number. Names, comments and openssl's text between the blocks are accepted, as in curl's `cacert.pem`.
- The proxy and chart mirror authorities are checked the same way, up to 256 KiB, and stored as the certificates alone.

### Fixed

- A patch or `kubectl edit` that drops a cluster's pod or service ranges no longer clears them.
