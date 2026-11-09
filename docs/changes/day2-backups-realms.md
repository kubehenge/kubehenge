### Changed

- A Proxmox VE backup of every VM leaves cluster nodes out (`backup=0`) and never freezes them, as etcd snapshots and Velero cover them.

### Fixed

- A user name with an `@` of its own, such as an Active Directory principal name, signs in with the chosen realm.
