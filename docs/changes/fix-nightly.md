### Fixed

- A kubeadm control plane is ready as soon as its first worker joins, not up to 10 minutes later.
- A kubeadm cluster without workers no longer reports its control planes as not ready.
