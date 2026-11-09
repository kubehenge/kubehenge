### Added

- Templates take VM options (`--cpu-type`, `--ssd`, `--discard`, `--iothread`, `--numa`) from `kubehenge template create` and `refresh`.
- The Build template dialog sets the same options under **Hardware**.
- A new build keeps the newest build's options unless told otherwise.
- Each template records its hardware in `hw-` tags, shown in the Templates view and `kubehenge template list`.
- Placement keeps a cluster off Proxmox VE nodes that cannot run its templates' CPU type.
- A cluster that no node can run, by its templates' CPU type, is refused.
- A cluster whose templates use NUMA is refused machines of one core, through the interface, kubectl and the CLI alike.
- Health warns when an etcd member's write-ahead log syncs have a 99th percentile above 25 ms, on every distribution.
- The docs have a sizing guide for etcd storage ([clusters.md](clusters.md#etcd-storage)).
- The docs say that GPUs, PCI passthrough, extra disks and huge pages are not available yet.

### Changed

- Clusters select their template with `matchPolicy: uniqueSubset`, so the hardware tags never change the selection.
