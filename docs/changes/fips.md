### Added

- RKE2 clusters can run their node OS in FIPS mode, `almalinux-10-fips` or `rocky-10-fips`, with `fips: true` in `nodeOS`.
- FIPS nodes boot with `fips=1` and the `FIPS` crypto policy, from VM templates of their own.
- FIPS mode runs on Canal alone, RKE2's only CNI built for it.
- k3s and kubeadm refuse FIPS mode, in the interface, the CLI and kubectl alike.
