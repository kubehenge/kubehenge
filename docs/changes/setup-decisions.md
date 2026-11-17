### Added

- `kubehenge login` signs in once, with a password or a token, and `kubehenge logout` ends it. `appliance` and `cluster` use it.

### Changed

- The setup preselects up to three nodes that answer and fills their addresses and the API address from the bridge's free addresses. You are asked only for the rest.
- The home page lists the clusters once, without the fleet rail beside it. Every other page keeps the rail.
- The sign-in flags and variables of `appliance` and `cluster` are gone, and `cluster` no longer reads `KUBEHENGE_PROXMOX_TOKEN`.
