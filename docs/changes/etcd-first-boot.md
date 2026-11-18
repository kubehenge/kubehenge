### Fixed

- A new Talos cluster no longer stays stuck when a failed image pull stops etcd on a control-plane node. Kubehenge bootstraps the first node again or reboots a joining one, and says why in an event.
- The management cluster's setup no longer times out on the same fault. The appliance starts etcd again the same way and says so in the run's log.
