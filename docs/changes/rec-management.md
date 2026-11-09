### Added

- A warning stands while the management etcd backups are off and repeats weekly. An administrator can dismiss it for 30 days.
- **Export** and `kubehenge appliance export` save the appliance's state, encrypted. **Restore from an export** and `kubehenge appliance import` read it back.
- **Recreate…** and `kubehenge appliance etcd-backups recreate` make the management cluster's VMs again and restore a snapshot.
- A Cluster whose VMs are all gone shows **VMs missing**. VMs that no Cluster owns show as **Unowned VMs**.
- Each cluster's spec, with its credentials, keys and certificate authorities, is stored in the backup target on every change. It is sealed by a key the appliance export carries.
- The fleet warns that an export is due once the recovery key exists and no export was taken, since stored Secrets open only with this appliance's key.
- Backup settings and `etcd-backups set-up --spec-recipient` take extra age public keys to seal specs to. `create --from --identity` opens those Secrets offline.
- `kubehenge cluster export` and `create --from` say whether Secrets are included. A Secret that cannot be opened stops `create --from` unless `--without-secrets` is given.
