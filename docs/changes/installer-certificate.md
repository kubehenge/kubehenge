### Added

- The installer asks the DNS name people open the interface at and checks it in DNS. The summary shows the address and says when the name has no record or points elsewhere.
- The installer asks which certificate the interface serves. A certificate of your own may be PEM, DER, PFX or P7B files, one or several, encrypted or not, and its chain is put in order ([upgrading.md](upgrading.md#the-appliances-certificate)).
- Where the Proxmox VE API's certificate comes from another CA, the installer says browsers that trust it will warn about one made here, and offers your own.
- `--cert-passphrase-file` gives the passphrase of encrypted certificate files.
- Administrators replace the interface's certificate with files in any format, make a self-signed one, get the node command for a cluster-CA one, or go back to the installer's ([upgrading.md](upgrading.md#from-the-interface)).
- The appliance can sign the interface's certificate with a CA of its own, held to the appliance's names and addresses. Trust that CA once and browsers stop warning ([upgrading.md](upgrading.md#the-appliances-own-ca)).
- `kubehenge appliance certificate` does the same from the CLI. A change reaches the next connection with no restart.
- Administrators change the interface's public address on the certificate page or with `kubehenge appliance certificate address`. Certificates the appliance made follow the new name ([upgrading.md](upgrading.md#the-public-address)).
- `kubehenge appliance certificate acme` gets the certificate from Let's Encrypt or your own ACME server and renews it. Proof is on port 443, port 80 or by DNS-01 through any DNS provider ([upgrading.md](upgrading.md#acme)).
- The certificate page sets up ACME, shows the order's state and orders again on request.
- The installer offers ACME as a third certificate choice, and `--acme` gives it without questions. The install ends on the certificate the first order issued, or says why it is still running or failed.
- The appliance listens on port 80 for ACME's http-01 proof and sends every other request to https.

### Changed

- `--cert` takes one or more files in any format, and `--key` is optional.
- `install.sh --certificate` removes a certificate given through the interface.
- `kubehenge login` trusts a certificate its `--ca` or the system's trust store vouches for. In the appliance it trusts the certificate the interface serves without `--ca`.

### Fixed

- Rancher's agent in a registered cluster runs its helm operations in the shell image Rancher runs, pinned by digest, rather than its own default named by tag.
