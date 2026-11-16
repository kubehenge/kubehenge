### Changed

- The setup's **Network** step shows the site's mirrors, proxy, CA and time zone in a **Site** group instead of under **Advanced**. Only the pod and Service ranges stay in **Advanced**.
- **Administration** lists its sections as links and keeps **Apply** in view while the form scrolls.
- **Edit settings** keeps the Talos image, cloud-init storage and bridge as saved and marks one no node offers. A node that does not answer no longer empties those lists.
- The setup preselects up to three nodes that answer and fills their addresses and the API address from the bridge's free addresses. You are asked only for the rest.
- Node addresses in the setup take the bridge's prefix length when you leave it out, except on a VLAN, where the tagged network is not the bridge's.
- The home page lists the clusters once, without the fleet rail beside it. Every other page keeps the rail.
- An empty existing passphrase asks for the state passphrase, not for 16 characters.
- **Remove Kubehenge** is disabled while clusters exist and says how many to delete.
- Token, certificate and unreachable-node messages end on what to do.
- `kubehenge --help` lists only the commands an administrator runs, and `cluster create --help` has examples and names the wizard's **Copy as YAML**.
- `kubehenge login` signs in once, with a password or a token, and `kubehenge logout` ends it. `appliance` and `cluster` use it.
- The sign-in flags and variables of `appliance` and `cluster` are gone, and `cluster` no longer reads `KUBEHENGE_PROXMOX_TOKEN`.
- The demo signs in again with any token after signing out, and answers the setup's final check.
