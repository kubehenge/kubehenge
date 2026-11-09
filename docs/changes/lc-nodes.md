### Added

- A worker pool takes labels and taints that its nodes register with, so a replaced node keeps them.
- Pool labels and taints are set when adding a pool, in its dialog, in `workerNodes` or with `kubehenge cluster pool set --label --taint`.
- Changing a pool's labels or taints replaces its nodes.
- The Health page notes a worker whose taints differ from its pool's setting.
- CIS hardening can be switched on after creation, in **Options** and with `kubehenge cluster update --cis`.
- The CIS dialog lists the running pods that the `restricted` level would refuse.
- Every kubeadm, RKE2 and k3s node image holds chrony, enabled, and the bake checks that it answers.
- The Health page warns about a node whose clock is over 30 seconds off the controller's.

### Changed

- Switching CIS hardening off is refused.
