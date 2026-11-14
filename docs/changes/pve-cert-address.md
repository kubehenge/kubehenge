### Fixed

- **Certificate address:** where Proxmox VE's certificate lacks the API's address, the installer, sign-in and token checks name what it lists and the fix ([install.md](install.md#an-address-the-certificate-lacks)).
- The setup check refuses a Proxmox VE address the management cluster's nodes cannot reach, such as an IPv6 one for nodes without IPv6.
