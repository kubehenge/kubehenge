### Added

- A cluster's nodes take registry mirrors, trusted CAs and a proxy on every distribution and node OS ([clusters.md](clusters.md#sites-with-limited-access)).
- The create wizard and the cluster page offer these settings, and a change replaces the nodes one at a time.
- `registryMirrors` lists up to 4 mirror addresses per registry, with an optional authority, for Talos, kubeadm, RKE2 and k3s.
- `trustedCAs` puts PEM certificates into Talos's trusted roots or the node OS's trust store.
- Kubehenge refuses private keys, expired certificates and text around them in `trustedCAs`.

### Changed

- `egressProxy` serves the nodes as well as the add-ons and gains `httpsUrl`.
- The cluster's own networks and API address join the proxy's exceptions.
- `egressProxy` values take letters, digits and a few separators only.
