### Added

- Chart mirrors serve the clusters' Helm charts by repository host, with their own certificate authority, and `kubehenge charts` lists what to copy ([install.md](install.md#mirroring-the-charts)).
- Cert-manager, Velero, Argo CD, Flux, Headlamp and Rancher go through a proxy (`egressProxy`), the management side's or one per cluster. Cluster ranges, `.svc` and `.cluster.local` bypass it.
- Argo CD and Flux trust a certificate authority for the Git server (`gitops.repository.caBundle`).
