# Releasing

A release is built by CI and published by a person. CI builds and tests everything, pushes the controller's and capmox's images and leaves a draft release; the release index that appliances upgrade from is signed offline with the release key, which never enters CI.

## What a release is

| Asset | What it is |
| --- | --- |
| `kubehenge_<version>_linux_amd64.tar.gz` | The appliance: the `kubehenge` binary, the management-cluster module with its charts, the shared Headlamp and the service units. The chart runs the controller image by digest. |
| `kubehenge_<version>_SHA256SUMS` | Checksums of the archive and of `install.sh`. |
| `install.sh` | The installer, carrying this release's version and archive checksum. |
| `index.json`, `index.json.sig` | Every published release with its archive's URL and checksum, and the signature appliances check it against. |
| `ghcr.io/kubehenge/kubehenge:v<version>` | The controller image, signed with Sigstore (keyless, by the release workflow) and with build provenance attached. |
| `ghcr.io/kubehenge/cluster-api-provider-proxmox:<capmox release>-<commit>` | While the management cluster runs capmox as built from an upstream commit (`capmox_build` in `management-cluster/versions.tf`): that build, signed and attested the same way. `hack/capmox-image.sh` builds it reproducibly and refuses an image whose digest differs from the one the module names, so anyone can rebuild it from that commit and compare. |

Every file the release workflow makes, and the image, carry GitHub build provenance, so anyone can check they were built from the tagged source by the release workflow. The index's signature, `index.json.sig`, is added afterwards, offline, with the release key, and is checked with that key instead.

## Once, before the first release

1. Make the release key on a machine that is not used for anything online, and keep the private key offline:

   ```sh
   kubehenge release keygen release.key
   ```

   It prints the public key; commit it as `pkg/release/release.pub`. Every build links it in, and an appliance installs only releases listed in an index signed by its private half. Losing the private key means no appliance can be upgraded from the interface again; whoever holds it can upgrade every appliance.
2. In the repository's settings, create the environment `release` with required reviewers, so a pushed tag waits for approval before the workflow runs.
3. After the first release, make the `kubehenge` and `cluster-api-provider-proxmox` packages on ghcr.io public.

## Each release

1. Set the version in `charts/kubehenge/Chart.yaml` (`appVersion`, and raise `version`), and write its section in `docs/changelog.md` (`## [<version>] - <date>`). The workflow refuses a tag without both.
2. Tag the commit and push the tag:

   ```sh
   git tag -s v<version> -m "Kubehenge <version>"
   git push origin v<version>
   ```

3. Approve the run. It runs every lint and unit test, builds the controller image and capmox's, signs them, builds the archive and `install.sh`, makes `index.json` from the previous release's signed index (checked against the release key) with this release added, and creates a draft release.
4. Check what CI built, on a machine with the GitHub CLI and cosign:

   ```sh
   gh release download v<version> --dir release
   cd release
   gh attestation verify kubehenge_<version>_SHA256SUMS --repo kubehenge/kubehenge
   sha256sum -c kubehenge_<version>_SHA256SUMS
   gh attestation verify kubehenge_<version>_linux_amd64.tar.gz --repo kubehenge/kubehenge
   gh attestation verify install.sh --repo kubehenge/kubehenge
   gh attestation verify index.json --repo kubehenge/kubehenge
   cosign verify ghcr.io/kubehenge/kubehenge:v<version> \
     --certificate-identity-regexp '^https://github.com/kubehenge/kubehenge/\.github/workflows/release\.yml@refs/tags/v' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   cosign verify "$(sed -n 's/^ *image *= *"\(ghcr.io\/kubehenge\/cluster-api-provider-proxmox@.*\)"/\1/p' ../management-cluster/versions.tf)" \
     --certificate-identity-regexp '^https://github.com/kubehenge/kubehenge/\.github/workflows/release\.yml@refs/tags/v' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   ```

   Read `index.json`: the previous releases unchanged, and one new entry whose checksum matches `SHA256SUMS`.
5. On the offline machine, sign the index and bring back only the signature:

   ```sh
   kubehenge release sign release.key index.json
   ```

   This writes `index.json.sig`, after checking the index parses and the signature verifies.
6. Upload the signature and publish:

   ```sh
   gh release upload v<version> index.json.sig
   gh release edit v<version> --draft=false --latest
   ```

Appliances look for `releases/latest/download/index.json`; until the signature is published beside it, they offer nothing, so a release published without it upgrades no appliance.
