### Before you upgrade

- `kubectl delete`, a GitOps prune and a namespace delete are refused until the cluster carries the annotation `kubehenge.dev/allow-deletion` ([clusters.md](clusters.md#delete-with-kubectl-or-gitops)). The interface and the CLI set it.

### Fixed

- A cluster that no longer meets a rule, such as one whose trusted authority expired, still takes annotations, labels, finalizers and changes to its other settings.
- While the controller is away, a cluster still takes labels, finalizers and `kubehenge.dev/allow-deletion`, so it can be deleted.
