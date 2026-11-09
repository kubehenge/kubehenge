### Added

- Templates take VM options (`--cpu-type`, `--ssd`, `--discard`, `--iothread`, `--numa`) from `kubehenge template create`, `refresh` and the Build template dialog's **Hardware**. A new build keeps the newest's.
- Each template records its hardware in `hw-` tags, shown in the Templates view and `kubehenge template list`.
- Placement keeps a cluster off Proxmox VE nodes that cannot run its templates' CPU type, and refuses a cluster that no node can run.
- A cluster whose templates use NUMA is refused machines of one core, through the interface, kubectl and the CLI alike.
- Health warns when an etcd member's write-ahead log syncs have a 99th percentile above 25 ms, on every distribution ([clusters.md](clusters.md#etcd-storage) has a sizing guide).
- The docs say that GPUs, PCI passthrough, extra disks and huge pages are not available yet.

### Changed

- Clusters select their template with `matchPolicy: uniqueSubset`, so the hardware tags never change the selection.
