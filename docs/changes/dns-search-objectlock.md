### Added

- Clusters take search domains with their name servers, so a pod resolves `git` as `git.corp.example`. The create wizard, `network.searchDomains` and the management cluster's settings carry them.
- Both wizards fill Search domains from the Proxmox VE node's own search domain until you edit the field.

### Fixed

- An etcd snapshot is one object with a small JSON record beside it, so a bucket with object lock no longer keeps two locked copies, and a prune deletes both objects.
