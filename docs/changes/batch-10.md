### Fixed

- A Site CA or proxy certificate of just a TLS inspection root now works for the management nodes. Setup names the certificate that breaks a chain before Apply.
- A failed run names the node, host and issuer of a rejected certificate.
- A management cluster with a Site CA, a proxy CA or a chart mirror's CA now sets up the Add-on Provider for Helm.

- A first setup that failed behind TLS inspection now finishes on the next Apply once the Site CA is set. The appliance reboots the nodes whose etcd or kubelet gave up pulling their images.

### Internal

- The end-to-end tests run behind TLS inspection and behind a credentialed and an inspecting proxy. A download that ignores the Site CA or the proxy setting fails its lane.
