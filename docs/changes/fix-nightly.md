### Fixed

- A kubeadm control plane is ready as soon as its first worker joins, not up to 10 minutes later.
- A kubeadm cluster without workers no longer reports its control planes as not ready.
- Workers of a Calico or Cilium cluster need room for the pods the CNI runs once per cluster, so workloads are not left pending.
- The interface asks the same worker memory for the chosen CNI as the controller does.
