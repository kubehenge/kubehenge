### Added

- **Apply now**, `kubehenge cluster apply-addon-changes` or `kubehenge.dev/apply-addon-changes` apply waiting add-on changes at once.
- `useExisting.certManager` uses the cluster's own cert-manager, and the condition `AddonConflict` names a clashing definition.
- The docs list each add-on's namespaces, privileges and registries, and `namespaceLabels` labels the add-on namespaces before their first pod.

### Changed

- Add-on chart and value changes from a new release wait for the cluster's upgrade window, shown on its page and in the Upgrade dialog.
- Kubehenge no longer installs cert-manager, Kyverno or monitoring over a cluster's own install.
