### Added

- Worker pools take labels and taints that nodes keep when replaced, set in the pool dialog, `workerNodes` or `kubehenge cluster pool set --label --taint`. Changing them replaces the nodes.
- The Health page notes a worker whose taints differ from its pool's setting.
- CIS hardening can be switched on after creation, in **Options** and with `kubehenge cluster update --cis`, but not off. The dialog lists running pods that the `restricted` level would refuse.
- Every kubeadm, RKE2 and k3s node image holds chrony, enabled, and the bake checks that it answers.
- The Health page warns about a node whose clock is over 30 seconds off the controller's.
