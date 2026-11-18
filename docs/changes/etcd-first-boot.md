### Fixed

- A new Talos cluster no longer stays stuck when a failed image pull stops etcd on a control-plane node. Kubehenge bootstraps the first node again or reboots a joining one, and says why in an event.
