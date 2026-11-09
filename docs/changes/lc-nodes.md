### Before you upgrade

- Build every node template again before nodes are added or replaced. kubeadm, RKE2 and k3s nodes wait at boot for a chrony that older templates do not set up.

### Added

- Worker pools take labels and taints that nodes keep when replaced, set in the pool dialog, `workerNodes` or `kubehenge cluster pool set --label --taint`. Changing them replaces the nodes.
- The Health page notes a worker whose taints differ from its pool's setting.
- CIS hardening can be switched on after creation, in **Options** and with `kubehenge cluster update --cis`, but not off. The dialog lists running pods that the `restricted` level would refuse.
- Every kubeadm, RKE2 and k3s node image runs chrony, checked when the template is built.
- The Health page warns about a cluster node whose clock is over 30 seconds off the controller's.
