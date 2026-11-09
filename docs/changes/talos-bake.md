### Security

- Talos templates are baked on your Proxmox VE host from Sidero's images, each pinned by digest. They are no longer imported from the Image Factory without a checksum.
- Only pinned Talos releases are offered and admitted for a new cluster or a change of release.

### Changed

- New Talos machines clone only baked templates. The template refresh deletes the ones made from the Image Factory once nothing waits on them.
