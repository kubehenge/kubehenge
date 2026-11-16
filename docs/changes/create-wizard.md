### Added

- A cluster whose disks do not fit its thick storage gets a `StorageLacksSpace` condition, also when made with kubectl or GitOps.

### Changed

- **Create cluster** starts from **Defaults**. After you choose a cluster to copy, each copied value is marked **Copied from**.
- etcd snapshots stay on with the first backup target, and their access key is optional.
- Confirm shows the firewall's state.
- A mistyped gateway is marked without moving the addresses.
- The wizard has a **Policies** step for the egress proxy, CIS hardening, Pod security and automatic upgrades.
- Storing a cluster's own access key in the wizard needs `Sys.Modify` on the workload pool. Without a key, etcd snapshots and Velero wait, and the wizard says so.
- A credential Secret made with kubectl is never deleted by Kubehenge or owned by its Cluster.
- The wizard starts with Velero backups on where the class offers a target, and with automatic patch upgrades. It starts with three control-plane nodes where Proxmox VE has three nodes.
- **Update node images** can be switched off when a cluster is created.
- The egress proxy offers **Management proxy** only when the management cluster has one.

### Fixed

- The wizard refuses a cluster name already in use on the first step.
- Machine sizes are checked against the nodes and storage on the Machines step. A machine larger than any node's memory, or disks beyond a thick storage's free space, are refused there and by the API.
- Confirm shows machine sizes in GiB and TiB.
- The load-balancer example follows the gateway.
- The access key hint names its bucket folder.
- A node OS that is not released or has no VM template says so in its option, such as `(no template yet)`. A problem on VLAN tag, MTU or Queues replaces its **Optional** hint.
- Asking for a missing VM template says the build takes its address from DHCP unless the settings give a build address.
