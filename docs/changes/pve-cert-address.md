### Fixed

- When Proxmox VE's certificate lacks the API's address, the installer and sign-in name what it lists and the fix. They no longer blame the CA or say "Proxmox VE did not answer" ([install.md](install.md#an-address-the-certificate-lacks)).
- The setup check refuses a Proxmox VE address the management cluster's nodes cannot reach, such as an IPv6 one for nodes without IPv6.
