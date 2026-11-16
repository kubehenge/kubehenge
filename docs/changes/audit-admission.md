### Fixed

- A cluster's `storage`, `controlPlaneEndpoint`, bridge, VLAN and pod and service ranges cannot change once it exists, through kubectl and GitOps as in the interface.
- kubectl and GitOps now refuse what the interface refuses: a spec change on a cluster being deleted, an empty `allowedNodes` list, and a node listed twice or one Proxmox VE lacks.
- kubectl and GitOps also refuse a VLAN tag on a VNet whose zone tags its traffic already.
- Finalizers, labels and annotations still change on a cluster being deleted.
