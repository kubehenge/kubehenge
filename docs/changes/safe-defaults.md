### Changed

- A Kubehenge cluster is deleted only once it carries `kubehenge.dev/allow-deletion`. kubectl, GitOps prunes and deleted namespaces meet the guard. The interface and the CLI set it.

### Fixed

- A cluster that no longer meets a rule, such as one whose trusted authority expired, still takes annotations, labels, finalizers and changes to its other settings.
