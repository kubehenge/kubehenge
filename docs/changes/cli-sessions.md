### Changed

- A password login by `kubehenge login` lasts seven days, set by `--cli-session-max-age`. Browser sign-ins keep their eight hours ([Sessions](architecture.md#sessions)).
- On the appliance, a password login by `kubehenge login` outlives upgrades and certificate renewals of the interface ([clusters.md](clusters.md#signing-in-and-finding-clusters)).
- A new `kubehenge login` ends the password login it replaces. A command still running carries on with the new login at the same address.
- `kubehenge logout` says when it could not end the session at the interface. It forgets the login all the same.
