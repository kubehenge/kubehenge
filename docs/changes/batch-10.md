### Fixed

- A first setup that failed behind TLS inspection now finishes on the next Apply once the Site CA is set. The appliance reboots the nodes whose etcd or kubelet gave up pulling their images.
