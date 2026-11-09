### Added

- The weekly build makes the kubeadm template of each running minor's newest patch release, and of the next minor.
- `template_refresh.kubeadm_patches` switches those kubeadm builds off.
- Administrators pin RKE2 and k3s releases from Administration or with `kubehenge release pin`, after seeing the checksum digests.
- Each release pin records who added it.

### Changed

- A build with an older kernel than the previous build's stays out of what clusters move to, and Templates marks it.
- The controller builds such a template again an hour later, 3 times at most.
