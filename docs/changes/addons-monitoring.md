### Added

- The monitoring add-on takes alert receivers (email, webhook, Slack, Teams), a remote write target and external Alertmanagers. Their secrets are stored as credentials.
- Switching monitoring off keeps its volumes, as its dialog says. The docs list what monitoring, logs and Headlamp leave behind.

### Changed

- Prometheus reads every ServiceMonitor, PodMonitor, Probe, ScrapeConfig and PrometheusRule of the cluster, not only the release's.
- Monitoring installs and upgrades bring the Prometheus Operator's CRDs up to date.
