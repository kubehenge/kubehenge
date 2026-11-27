### Fixed

- `kubehenge template create` builds through the proxy its environment names, and trusts the authorities in `KUBEHENGE_PROXY_CA_FILE` when it fetches repository keys.

### Internal

- The workload tests behind TLS inspection give the management cluster's module the root and its renamed copy, as the appliance does, so its nodes pull etcd.
