### Added

- `kubehenge appliance ready-for-reboot` exits 0 only when every etcd member is healthy. With `--node NODE`, it exits 0 when stopping that node's guests leaves every etcd a majority of healthy members.
- A blocked reboot prints its first blocker in one line, with the cluster, the member and the node.
- Administration, Health shows the same answer for each Proxmox VE node under Ready for reboot.
- Patching Proxmox VE hosts in [install.md](install.md) says to check, reboot one host, wait for the check to pass and go on.
- A cluster whose API is silent for 10 minutes, with under a majority of control-plane VMs running, gets `ControlPlaneQuorumLost`. An event of that name and a Health item come with it.
- Recover… accepts that condition besides a stalled roll, through the interface, the CLI and kubectl. It lists the control planes whose VM runs, and says that what only the lost members held is gone.
- The troubleshooting page covers recovering a cluster that lost two of three control planes, with the kubectl annotation.

### Changed

- The management VMs get `startup: order=1,up=90`, so they start first after a host boots.
- The controller refuses an etcd reset to one machine unless the roll stalled or the quorum is lost, for kubectl and GitOps too. The restore status gives the reason.

### Internal

- A Talos cluster's etcd reset is tested after the loss of two of three control planes, as the kubeadm, RKE2 and k3s resets are.
