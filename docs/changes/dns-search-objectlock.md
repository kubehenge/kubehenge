### Added

- Clusters take search domains with their name servers, so a pod resolves `git` as `git.corp.example`. The create wizard, `network.searchDomains` and the management cluster's settings carry them.
- The create wizard and the management cluster's setup fill **Search domains** from the Proxmox VE node's own until you edit the field.

### Fixed

- A bucket with object lock no longer keeps two locked copies of each etcd snapshot, and pruning deletes all of a snapshot's objects.
