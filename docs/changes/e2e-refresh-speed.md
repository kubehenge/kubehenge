### Changed

- A node image refresh or hold starts once the cluster's `upgrades` change, it settles or its class gets a new build, with no wait of up to five minutes.
- A template build with no work for 10 minutes (no new log line, under 1 MiB received, 16 MiB written, idle CPUs) fails and is tried again, where it held the run for 45 minutes.
