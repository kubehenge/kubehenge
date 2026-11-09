### Added

- Clusters take search domains with their name servers, so a pod resolves `git` as `git.corp.example`.
- The create wizard, `network.searchDomains` and the management cluster's settings take search domains.
- Both wizards fill Search domains from the Proxmox VE node's own search domain until you edit the field.

### Changed

- An etcd snapshot is one object with a small JSON record beside it, where it was the object copied onto itself.

### Fixed

- A bucket with object lock no longer keeps two locked copies of every etcd snapshot.
- A prune deletes both of a snapshot's objects.
