### Added

- The installer asks for the DNS name people open the interface at and checks it. The summary says when the name has no record or points elsewhere.
- The installer asks which certificate the interface serves. Your own takes PEM, DER, PKCS#7 or PKCS#12 files, encrypted or not (`--cert-passphrase-file`).
- The installer says browsers warn about its certificate until they trust the Proxmox VE CA. Where another CA signed the node's API certificate, it preselects your own files.
- Administrators replace the interface's certificate on its page or with `kubehenge appliance certificate`, with no restart.
- The appliance can sign the interface's certificate with its own CA, which may sign only for the appliance's names and addresses.
- Administrators change the interface's public address on the certificate page or with `kubehenge appliance certificate address`. Certificates the appliance made follow it.
- The certificate page and `kubehenge appliance certificate acme` order the certificate from Let's Encrypt or your own ACME server and renew it. They prove the name on port 443, port 80 or by DNS record.
- A DNS provider without its credentials is refused, and the refusal names the variables it needs, such as `CLOUDFLARE_DNS_API_TOKEN`.
- The installer offers ACME as a third certificate choice, and `--acme` skips the questions. At the end it names the issuer, or says why the first order is still running or failed.
- The appliance listens on port 80 for ACME's http-01 proof. It sends every other request there to https.

### Changed

- `--cert` takes one or more files in any format. `--key` is optional.
- `install.sh --certificate` removes a certificate given through the interface.
- `kubehenge login` trusts a certificate that its `--ca` or the system's trust store vouches for. In the appliance it needs no `--ca`.

### Fixed

- Rancher's agent in a registered cluster runs its Helm operations in the shell image Rancher pins, not a default named by tag alone.
- A template build finds its pinned AlmaLinux or Rocky Linux image in the vault once the mirror lists only newer builds.
