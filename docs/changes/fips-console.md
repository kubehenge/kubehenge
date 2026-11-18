### Changed

- AlmaLinux and Rocky Linux node templates are built from 10.3 packages, which carry the current security fixes.

### Fixed

- FIPS node templates on AlmaLinux and Rocky Linux 10.3 boot. Their initramfs found no checksum of the kernel and switched the node off.
