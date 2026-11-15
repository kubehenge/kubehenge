### Added

- A **Site CA** in Setup and Administration, and `siteCA` in the chart values. Nodes, the release mirror, backup targets and identity providers trust it unless they name their own.
- Saving a new site CA says how many clusters replace their nodes. At most four roll at once, each inside its upgrade window where it has one.
- A paused cluster takes a new site CA once unpaused, and the release mirror's authority reaches nodes with the site CA their cluster was given.
- An expired or damaged site CA is reported on each cluster and stops no controller. A damaged one changes no cluster's site CA until it is fixed.

### Changed

- A release mirror certificate that is damaged or expired is reported on each cluster and no longer stops the controller.
- A certificate bundle refuses a damaged BEGIN or END line, naming its line. Names, comments and openssl's text between the blocks are accepted, as in curl's `cacert.pem`.
- The interface accepts and refuses the same certificate bundles as the server.
- A cluster's certificate bundles are stored as plain certificates on every path, kubectl and GitOps included. Names and comments between the blocks are dropped.
- The proxy and chart mirror authorities follow the same layout rule, up to 256 KiB.
