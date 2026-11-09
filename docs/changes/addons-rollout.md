### Added

- **Apply now**, `kubehenge cluster apply-addon-changes` or `kubehenge.dev/apply-addon-changes` apply waiting add-on changes at once.
- The condition `AddonConflict` names the definition that clashes with an add-on.
- `useExisting.certManager` uses the cluster's own cert-manager.
- The docs list what each add-on owns, with its namespaces, privileges and registries.
- `namespaceLabels` labels the add-on namespaces before their first pod.

### Changed

- Add-on chart and value changes from a new release wait for the cluster's upgrade window, shown on its page and in the Upgrade dialog.
- Kubehenge no longer installs cert-manager, Kyverno or monitoring over a cluster's own install.
