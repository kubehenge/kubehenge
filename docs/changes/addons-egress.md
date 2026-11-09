### Added

- Clusters can take their add-ons' Helm charts from a mirror per repository host, with its own CA. `kubehenge charts` lists what to copy ([install.md](install.md#mirroring-the-charts)).
- cert-manager, Velero, Argo CD, Flux, Headlamp and Rancher go out through a proxy (`egressProxy`), the management cluster's or the cluster's own. Cluster ranges, `.svc` and `.cluster.local` bypass it.
- Argo CD and Flux trust a certificate authority for the Git server (`gitops.repository.caBundle`).
