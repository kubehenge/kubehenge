### Changed

- A node image refresh or hold starts as soon as the cluster's `upgrades` change, the cluster settles or its class gets a new build, not up to five minutes later.
- A template build that makes no progress for 10 minutes fails and is tried again, where it held the run for 45 minutes.
