### Added

- The monitoring add-on takes alert receivers (email, webhook, Slack, Teams), a remote write target and external Alertmanagers.
- Their secrets are stored as credentials, and each is set in the interface, with kubectl or from the command line.
- The docs list what stays after monitoring, logs or Headlamp is switched off, and the monitoring dialog says the volumes stay.

### Changed

- Prometheus reads every ServiceMonitor, PodMonitor, Probe, ScrapeConfig and PrometheusRule of the cluster, not only the release's.
- The monitoring chart's own job applies the Prometheus Operator's CRDs on every install and upgrade, with images pinned by digest.
