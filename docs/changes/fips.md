### Added

- RKE2 clusters can run their node OS in FIPS mode (`almalinux-10-fips` or `rocky-10-fips`) with `fips: true` in `nodeOS`. Nodes boot with `fips=1` and the `FIPS` crypto policy from their own templates.
- FIPS mode runs on Canal alone, RKE2's only CNI built for it, and k3s and kubeadm refuse it in the interface, the CLI and kubectl alike.
