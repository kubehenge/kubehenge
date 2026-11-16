### Fixed

- The appliance records its addresses for the clusters' firewalls, and finds its own guest in Proxmox VE by its MAC address. Its service could not list its network interfaces.

### Internal

- The appliance's end-to-end test waits for the CSI plugin's detach of the backup volume to end before it lifts a management VM's protection.
