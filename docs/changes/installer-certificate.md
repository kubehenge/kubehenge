### Added

- The installer asks the DNS name people open the interface at and checks it in DNS. The summary shows the address and says when the name has no record or points elsewhere.
- The installer asks which certificate the interface serves. Your own may be PEM, DER, PFX or P7B files, one or several, encrypted (`--cert-passphrase-file`) or not ([upgrading.md](upgrading.md#the-appliances-certificate)).
- The installer says that browsers warn about the certificate it makes until they trust the Proxmox VE CA. Where another CA signed the node's API certificate, it offers your own files first.
- Administrators swap the interface's certificate on its page or with `kubehenge appliance certificate`, with no restart ([upgrading.md](upgrading.md#from-the-interface)).
- The appliance can sign the interface's certificate with a CA of its own, held to the appliance's names and addresses ([upgrading.md](upgrading.md#the-appliances-own-ca)).
- Administrators change the interface's public address on the certificate page or with `kubehenge appliance certificate address`. Certificates the appliance made follow the new name ([upgrading.md](upgrading.md#the-public-address)).
- The certificate page and `kubehenge appliance certificate acme` get the certificate from Let's Encrypt or your own ACME server and renew it. Proof is on port 443, port 80 or by DNS-01 ([upgrading.md](upgrading.md#acme)).
- A DNS provider missing its credentials is refused with the variables it needs, such as `CLOUDFLARE_DNS_API_TOKEN`.
- The installer offers ACME as a third certificate choice, or `--acme` without questions. The install ends on the certificate the first order issued, or says why it is still running or failed.
- The appliance listens on port 80 for ACME's http-01 proof. It sends every other request there to https.

### Changed

- `--cert` takes one or more files in any format. `--key` is optional.
- `install.sh --certificate` removes a certificate given through the interface.
- `kubehenge login` trusts a certificate its `--ca` or the system's trust store vouches for. In the appliance it trusts the certificate the interface serves without `--ca`.

### Fixed

- Rancher's agent in a registered cluster runs its helm operations in Rancher's pinned shell image. It used its own default, named by tag alone.
- A template build finds its pinned AlmaLinux or Rocky Linux image in the vault once the mirror lists only newer builds.
