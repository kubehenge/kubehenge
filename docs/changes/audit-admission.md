### Fixed

- **Fixed settings:** a change of `storage` or `controlPlaneEndpoint` on an existing cluster is refused through `kubectl` and GitOps too, as the interface refuses it.
- **Clusters being deleted:** a change to the spec of a cluster being deleted is refused through `kubectl` and GitOps too. Finalizers, labels and annotations still change.
- **Allowed nodes:** an empty list, a node listed twice and a node Proxmox VE lacks are refused through `kubectl` and GitOps too, as the interface refuses them.
- **VLAN tag:** a VLAN tag on a VNet whose zone tags its traffic already is refused through `kubectl` and GitOps too, as the interface refuses it.
