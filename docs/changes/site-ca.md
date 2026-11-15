### Added

- A **Site CA** in Setup and Administration, and `siteCA` in the chart values. Nodes, the release mirror, backup targets and identity providers trust it unless they name their own.
- Saving a new site CA says how many clusters replace their nodes. At most four roll at once, each inside its upgrade window where it has one.
- A paused cluster takes a new site CA once unpaused, and the release mirror's authority reaches nodes with the site CA their cluster was given.
- An expired site CA is reported on each cluster and stops no controller.

### Changed

- A release mirror certificate that is damaged or expired is reported on each cluster and no longer stops the controller.
- A certificate bundle refuses a damaged BEGIN or END line, naming its line. Names, comments and openssl's text between the blocks are kept, as in curl's `cacert.pem`.
