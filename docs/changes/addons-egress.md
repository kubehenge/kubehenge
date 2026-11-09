### Added

- Chart mirrors serve the clusters' Helm charts by repository host, with their own certificate authority ([install.md](install.md#mirroring-the-charts)).
- `kubehenge charts` lists every chart and digest to copy into a mirror.
- Cert-manager, Velero, Argo CD, Flux, Headlamp and Rancher go through a proxy, the management side's or the cluster's own (`egressProxy`).
- The cluster's ranges, `.svc` and `.cluster.local` bypass the add-ons' proxy.
- Argo CD and Flux trust a certificate authority for the Git server (`gitops.repository.caBundle`).
