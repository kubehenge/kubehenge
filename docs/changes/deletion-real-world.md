### Changed

- Deleting a machine's VM removes it from HA resources, backup jobs and replication jobs in one call, and drops disks left under its ID.
- A VM with Proxmox VE's protection flag is not destroyed, and the stalled deletion names the flag and `qm set <vmid> --protection 0`.

### Fixed

- A cluster's VM that an administrator renamed is destroyed with its machine, as a booted machine's SMBIOS UUID identifies its VM.
- A VM of another name under a machine's VM ID, with no UUID to check, is no longer released silently.
- Such a machine stays, with a warning event and a condition naming the VM ID and the VM found.
