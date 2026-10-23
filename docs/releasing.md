# Releasing

A pushed version tag releases Kubehenge with no further step. CI checks the tag, builds and tests everything, pushes the controller's and the providers' images, signs the release index that appliances upgrade from with the release key, signs and attests what it built, checks it all as a user would and publishes the release.

## What a release is

| Asset | What it is |
| --- | --- |
| `kubehenge_<version>_linux_amd64.tar.gz` | The appliance: the `kubehenge` binary, the management-cluster module with its charts, the shared Headlamp and the service units. The chart runs the controller image by digest. |
| `kubehenge_<version>_SHA256SUMS` | Checksums of the archive and of `install.sh`. |
| `install.sh` | The installer, carrying this release's version and archive checksum. |
| `index.json`, `index.json.sig` | Every published release with its archive's URL and checksum, and the signature appliances check it against. |
| `kubehenge_<version>_linux_amd64.spdx.json` | The archive's SBOM (SPDX 2.3): every file in it with its SHA-256, every Go module the `kubehenge` binary and Headlamp's server were built from, the interface's npm packages and the OpenTofu providers the module installs, each at its exact version. |
| `kubehenge_<version>.intoto.jsonl` | The files' two signed attestations, build provenance and the archive's SBOM, as Sigstore bundles, so they verify from the file alone. |
| `ghcr.io/kubehenge/kubehenge:v<version>` | The controller image. |
| `ghcr.io/kubehenge/cluster-api-provider-proxmox:<capmox release>-<commit>` | While the management cluster runs capmox as built from an upstream commit (`capmox_build` in `management-cluster/versions.tf`) with the fixes in `scripts/capmox-patches`: that build. `scripts/provider-image.sh` builds it reproducibly and refuses an image whose digest differs from the one the module names, so anyone can rebuild it from that commit and those patches and compare. |
| `ghcr.io/kubehenge/cluster-api-addon-provider-helm:<release>-<commit>` | While the management cluster runs the Add-on Provider for Helm with the fixes in `scripts/helm-addon-patches` (`helm_addon_build`): that build, made as capmox's is. |

The Talos providers are not part of a Kubehenge release: the management cluster installs them from the releases of Kubehenge's forks, [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos), each of which signs and attests its own image and files.

## How it is built and signed

A pushed tag starts `.github/workflows/release.yml`, which calls `.github/workflows/release-build.yml` for everything that makes the release's bytes and signs them, then checks and publishes what comes out:

1. **gate** refuses to go on unless the tag is annotated and carries a signature GitHub verifies, names a commit of main that carries one too, and that commit passed every check main's ruleset requires, read from the merge queue's run on that very commit, the end-to-end tests among them. The tag has to match the chart's `appVersion`, `docs/changelog.md` needs a section for the version, and `pkg/release/release.pub` must be there.
2. **test** runs every lint and unit test and the integration tier again, so that a vulnerability published since the merge, in a Go module or an action, stops the release.
3. **build** pushes the three images and builds the archive, `install.sh` and `index.json`, the last made from the previous release's signed index (checked against the release key) with this release added.
4. **sign** signs `index.json` with the release key, writing `index.json.sig`. It is the only job that holds the key, the `RELEASE_KEY` secret of the `release` environment, and it checks out nothing: it checks the archive against its checksum and signs with the `kubehenge` binary inside it, which refuses any key but the one it was built to trust.
5. **sbom** writes the SPDX SBOMs of the archive and of each image with syft (`scripts/sbom.sh`, `scripts/syft.yaml`), and checks each lists what ships: every file of the archive with its SHA-256, every module `go version -m` finds in its binaries, the interface's packages and the module's providers at their locked versions; for an image, the Go module its program is built from and the base image's packages.
6. **attest** signs the images with Sigstore (keyless) and attests build provenance and the SBOMs: of the files, onto the repository and into `kubehenge_<version>.intoto.jsonl`; of the images, onto the repository and beside each image in ghcr.io. It is the only job that can sign: it holds the OpenID Connect token Sigstore signs against, and runs no code but cosign, which it installs by its checksum, and the attest action.
7. **verify** checks everything as a user would, from the files alone, naming `release-build.yml` at this tag as the builder: the checksums, each file's provenance, `index.json.sig`'s among them, the archive's SBOM, and each image's provenance, SBOM and signature.
8. **publish** creates the release with every file, publishes it and marks it the latest. Its notes are the version's changelog section; before 1.0.0 they open with the README's alpha warning. A draft left by a failed run is made again; a published release is never rewritten.

This is SLSA Build Level 3 on GitHub Actions: the build runs on GitHub-hosted runners, used once, in a reusable workflow, so the certificate Sigstore issues names `release-build.yml` rather than whatever called it, and a verifier that insists on it (`--signer-workflow`, `--certificate-identity`) insists on its exact steps at the tag. No caller can change those steps, and no build cache, which another workflow's run could have written, reaches the release.

### What is attested

| Subject | Attestations | Stored |
| --- | --- | --- |
| The archive | build provenance (SLSA v1), SBOM (SPDX 2.3) | on the repository, and in `kubehenge_<version>.intoto.jsonl` |
| `kubehenge_<version>_SHA256SUMS`, `install.sh`, `index.json`, `index.json.sig`, the archive's SBOM | build provenance | on the repository, and in `kubehenge_<version>.intoto.jsonl` |
| Each of the three images | build provenance, SBOM, cosign signature | on the repository, and beside the image in ghcr.io |
| `index.json` | the release key's signature, `index.json.sig` | the release |

## Once, before the first release

1. Make the release key:

   ```sh
   kubehenge release keygen release.key
   ```

   It prints the public key; commit it as `pkg/release/release.pub`. Every build links it in, and an appliance installs only releases listed in an index signed by its private half. The same command makes a new key whenever one is wanted; appliances then upgrade only to releases built with the new `release.pub`.
2. In the repository's settings:
   - create the environment `release`, deployable only from tags matching `v*`, and give it the secret `RELEASE_KEY` holding the contents of `release.key`;
   - add a tag ruleset for `v*` that blocks updates and deletions, so a released tag can never be moved;
   - turn on immutable releases (Settings, General, Releases), so a published release's files and tag can never change and GitHub attests the release itself;
   - add the signing key you tag with to your GitHub account as a signing key, so GitHub verifies the tag's signature.
3. After the first release, make the `kubehenge`, `cluster-api-provider-proxmox` and `cluster-api-addon-provider-helm` packages on ghcr.io public.

## Each release

1. Set the version in `charts/kubehenge/Chart.yaml` (`appVersion`, and raise `version`), and write its section in `docs/changelog.md` (`## [<version>] - <date>`), in a pull request. The workflow refuses a tag without both.
2. Once it is merged and main's checks have finished, tag main's commit and push the tag:

   ```sh
   git tag -s v<version> -m "Kubehenge <version>"
   git push origin v<version>
   ```

   That is all: the run goes through the steps [above](#how-it-is-built-and-signed) and publishes the release. Appliances look for `releases/latest/download/index.json` and its signature, so they offer the new release as soon as it is published.

## Checking a release

Anyone can check a published release, on a machine with the GitHub CLI and cosign, from a checkout of the tag (for the image references in `management-cluster/versions.tf`):

```sh
gh release verify v<version> --repo kubehenge/kubehenge
gh release download v<version> --repo kubehenge/kubehenge --dir release
cd release
sha256sum -c kubehenge_<version>_SHA256SUMS
verify() {
  gh attestation verify "$@" --repo kubehenge/kubehenge \
    --signer-workflow kubehenge/kubehenge/.github/workflows/release-build.yml \
    --source-ref refs/tags/v<version> --deny-self-hosted-runners
}
for file in kubehenge_<version>_linux_amd64.tar.gz kubehenge_<version>_SHA256SUMS install.sh index.json \
  index.json.sig kubehenge_<version>_linux_amd64.spdx.json; do
  verify "$file" --bundle kubehenge_<version>.intoto.jsonl
done
verify kubehenge_<version>_linux_amd64.tar.gz --bundle kubehenge_<version>.intoto.jsonl \
  --predicate-type https://spdx.dev/Document/v2.3
for image in ghcr.io/kubehenge/kubehenge:v<version> \
  "$(sed -n 's/^ *image *= *"\(ghcr.io\/kubehenge\/cluster-api-provider-proxmox@.*\)"/\1/p' ../management-cluster/versions.tf)" \
  "$(sed -n 's/^ *image *= *"\(ghcr.io\/kubehenge\/cluster-api-addon-provider-helm@.*\)"/\1/p' ../management-cluster/versions.tf)"; do
  verify "oci://$image"
  verify "oci://$image" --predicate-type https://spdx.dev/Document/v2.3
  cosign verify "$image" \
    --certificate-identity https://github.com/kubehenge/kubehenge/.github/workflows/release-build.yml@refs/tags/v<version> \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
done
```

`gh release verify` checks GitHub's attestation of the published release: its tag and every file as they now stay. `make verify-providers`, from the same checkout, checks the Talos providers the tag pins the same way, against their forks' release workflows. Until the packages on ghcr.io are public, the image checks need a login (`docker login ghcr.io`). `index.json` should hold the previous releases unchanged and one new entry whose checksum matches `SHA256SUMS`.

How users check a release before installing it is in [install.md](install.md#verifying-a-release).
