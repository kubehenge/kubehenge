### Changed

- A password login by `kubehenge login` lasts seven days, set by `--cli-session-max-age`. Browser sign-ins keep their eight hours ([Sessions](architecture.md#sessions)).
- On the appliance, a password login by `kubehenge login` outlives upgrades and certificate renewals of the interface ([clusters.md](clusters.md#signing-in-and-finding-clusters)).
