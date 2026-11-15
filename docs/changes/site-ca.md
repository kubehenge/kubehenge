### Added

- A **Site CA** in Setup and Administration, and `siteCA` in the chart values. Nodes, the release mirror, backup targets and identity providers trust it unless they name their own.
- Saving a new site CA says how many clusters replace their nodes. At most four roll at once, each inside its upgrade window where it has one. A paused cluster waits for its unpause, and the release mirror's authority reaches a cluster's nodes with the site CA it was given. The site CA is saved as bare PEM certificates, and an expired one is reported on each cluster without stopping the controller.
