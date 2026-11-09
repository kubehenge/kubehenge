### Added

- Security groups that would cut the appliance or the management cluster off the Proxmox VE API are refused.
- The setup's pre-flight reports such security groups too.
- Both name the addresses to add to the datacenter firewall's IPSet `management`.
