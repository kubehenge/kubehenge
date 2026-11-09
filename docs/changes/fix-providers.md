### Fixed

- The appliance applies the management cluster while GitHub turns away anonymous downloads, on a closed network too.
- The release archive carries the module's OpenTofu providers, and the appliance installs them from there alone.
- Each provider is still checked against the module's lock file, and the appliance tries none from the network.
