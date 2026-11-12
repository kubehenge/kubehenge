### Fixed

- After a k3s or RKE2 upgrade, a cluster keeps the admission policy of the distribution's Gateway API chart. Helm could delete it while upgrading the Traefik CRD chart.
