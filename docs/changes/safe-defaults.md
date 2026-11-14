### Changed

- A Kubehenge cluster is deleted only once it carries `kubehenge.dev/allow-deletion`. kubectl, GitOps prunes and deleted namespaces meet the guard. The interface and the CLI set it.
