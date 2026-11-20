### Added

- The installer asks the DNS name people open the interface at and checks it in DNS. The summary shows the address and says when the name has no record or points elsewhere.
- The installer asks which certificate the interface serves. A certificate of your own may be PEM, DER, PFX or P7B files, one or several, encrypted or not, and its chain is put in order ([upgrading.md](upgrading.md#the-appliances-certificate)).
- Where the Proxmox VE API's certificate comes from another CA, the installer says browsers that trust it will warn about one made here, and offers your own.
- `--cert-passphrase-file` gives the passphrase of encrypted certificate files.
- Administrators replace the interface's certificate with files in any format, make a self-signed one, get the node command for a cluster-CA one, or go back to the installer's ([upgrading.md](upgrading.md#from-the-interface)).
- `kubehenge appliance certificate` does the same from the CLI. A change reaches the next connection with no restart.

### Changed

- `--cert` takes one or more files in any format, and `--key` is optional.
- `install.sh --certificate` removes a certificate given through the interface.
- `kubehenge login` trusts a certificate its `--ca` or the system's trust store vouches for. In the appliance it trusts the certificate the interface serves without `--ca`.
