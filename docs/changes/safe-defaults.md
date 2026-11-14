### Changed

- A Kubehenge cluster is deleted only once it carries `kubehenge.dev/allow-deletion`, which the interface and the CLI set. kubectl, GitOps prunes and deleted namespaces meet the same guard.

### Fixed

- A cluster that no longer meets a rule, such as one whose trusted authority expired, still takes annotations, labels, finalizers and changes to its other settings.
