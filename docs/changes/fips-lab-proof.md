### Added

- FIPS mode stays unsupported until every part of the workload cluster test passes on a node OS ([templates.md](templates.md#fips-mode)).
- On `rocky-10-fips` with RKE2 and Canal, the test's `etcd` part passed and its `machines` part failed at its last step.
- The add-on parts of the test have not run in FIPS mode yet.
