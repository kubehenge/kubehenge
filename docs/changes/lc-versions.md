### Added

- The weekly build makes the kubeadm template of each running minor's newest patch release and of the next minor, and `template_refresh.kubeadm_patches` switches this off.
- Administrators pin RKE2 and k3s releases from Administration or with `kubehenge release pin`, after seeing the checksum digests. Each pin records who added it.

### Changed

- Clusters do not move to a build whose kernel is older than the previous build's, and **Templates** marks it. The controller builds again an hour later, 3 times at most.
