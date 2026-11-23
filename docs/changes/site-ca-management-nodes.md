### Fixed

- The management cluster's nodes trust the site CA. Behind TLS inspection without a proxy, they could not pull etcd's image, so the first setup never finished.
