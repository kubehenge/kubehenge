### Changed

- Every add-on that waits says what for in the cluster's `AddonsWaiting` condition, on its Health page and in `kubehenge cluster get`, with a Warning event when the list changes.
- A missing ACME external account binding key, DNS-01 credential or log output credential is a line in `AddonsWaiting` naming what to store. The DNS-01 issuer waits for its credential.
- The conditions `GatewayAPIReady` and `RancherRegistered` say what the Gateway API and Rancher registration wait for, or what failed.
- A kubelet serving certificate request left pending for an address that is not its Machine's gets a `ServingCertificatePending` Warning event naming the node and address.
- A cluster held before its VMs are created says why: the controller's error or what its add-ons wait for.
- A refresh of the interface shows the fleet as the tab last read it, greyed until read again, and no cluster as healthy before its health is read.
- `install.sh --upgrade` and `--certificate` find the cluster's appliance without `--ctid`. A plain `install.sh` upgrades the appliance on this node.

### Fixed

- `kubehenge template create` builds through the proxy its environment names, and trusts the authorities in `KUBEHENGE_PROXY_CA_FILE` when it fetches repository keys.
- A kubeadm template of a Kubernetes release whose images lack Kubernetes' signature is refused before its build, in the interface, the CLI and the refresh, naming the nearest signed release.
- capmox reads a slow `cloud-init status` on a busy host as still running, not failed. `ControlPlaneRollStalled` times a roll from its start, so a successor the health check keeps replacing counts.

### Internal

- The workload tests behind TLS inspection give the management cluster's module the root and its renamed copy, as the appliance does, so its nodes pull etcd.
