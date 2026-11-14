### Changed

- **Create cluster** starts from **Defaults**, and says **Copied from** the cluster you chose on the later steps. Etcd snapshots stay on with the first backup target, and their access key is optional.
- The wizard has a **Policies** step for the egress proxy, CIS hardening, Pod security and automatic upgrades.
- **Update node images** can be switched off when a cluster is created.
- The egress proxy offers **Management proxy** only when the management cluster has one, and Rancher registration only when a cluster runs Rancher.

### Fixed

- The wizard refuses a cluster name already in use on the first step.
- Machine sizes are checked against the nodes and storage on the Machines step. A machine larger than any node's memory, or disks beyond the storage's free space, are refused there.
- Machine sizes read in GiB and TiB on Confirm, the load-balancer example follows the gateway, and the access key hint names its bucket folder.
- Node OS options show a short state, and a problem on VLAN tag, MTU or Queues replaces its **Optional** hint.
- Asking for a missing VM template says the build takes its address from DHCP unless the settings give a build address.
