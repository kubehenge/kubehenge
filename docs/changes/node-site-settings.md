### Added

- Nodes take registry mirrors, trusted CAs and a proxy on every distribution and node OS, set in the wizard or on the cluster page, and a change replaces the nodes one at a time ([clusters.md](clusters.md#sites-with-limited-access)).
- `registryMirrors` lists up to 4 mirror addresses per registry, with an optional certificate authority, for Talos, kubeadm, RKE2 and k3s.
- `trustedCAs` puts PEM certificates into Talos's trusted roots or the node OS's trust store, and Kubehenge refuses private keys, expired certificates and text around them.
- `egressProxy` serves nodes as well as add-ons, takes an `httpsUrl` and lets the cluster's own networks and API address bypass it.
