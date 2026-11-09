### Added

- `kubehenge cluster timeouts`, the interface and the Cluster change a removed machine's drain and detach timeouts.

### Changed

- A removed machine waits at most 60 minutes for its node to drain and 10 for its volumes to detach.

### Fixed

- A disruption budget or a stuck detach no longer holds a rollout for ever.
