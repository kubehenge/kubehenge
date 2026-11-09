### Before you upgrade

- Add-on chart and value changes from a new release wait for the cluster's upgrade window, shown on its page and in the Upgrade dialog.

### Added

- **Apply now**, `kubehenge cluster apply-addon-changes` or `kubehenge.dev/apply-addon-changes` apply waiting add-on changes at once.
- `useExisting.certManager` makes the add-ons use the cluster's own cert-manager. The condition `AddonConflict` names an add-on that clashes with the cluster's own install.
- `namespaceLabels` labels the add-ons' namespaces before their first pod. The docs list each add-on's namespaces, privileges and registries.

### Changed

- Kubehenge no longer installs cert-manager, Kyverno or monitoring over a cluster's own install.
