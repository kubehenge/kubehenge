# Releasing

A pushed version tag releases Kubehenge with no further step. CI checks the tag, then builds, tests, signs and attests everything. It checks the result as a user would and publishes the release.

## What a release is

| Asset | What it is |
| --- | --- |
| `kubehenge_<version>_linux_amd64.tar.gz` | The appliance, made of the `kubehenge` binary, the management-cluster module with its charts, the shared Headlamp and the service units. The chart runs the controller image by digest. |
| `kubehenge_<version>_SHA256SUMS` | Checksums of the archive and of `install.sh`. |
| `install.sh` | The installer. It carries this release's version and archive checksum. |
| `index.json`, `index.json.sig` | Every published release with its archive's URL and checksum, when the index expires, and the signatures appliances check it against. |
| `kubehenge_<version>_linux_amd64.spdx.json` | The archive's SBOM (SPDX 2.3). It lists every file with its SHA-256. It also lists the Go modules of the `kubehenge` binary and Headlamp's server, the interface's npm packages and the module's OpenTofu providers, at their exact versions. |
| `kubehenge_<version>.intoto.jsonl` | The files' build provenance and the archive's SBOM, as signed Sigstore bundles that verify from the file alone. |
| `ghcr.io/kubehenge/kubehenge:v<version>` | The controller image. |
| `ghcr.io/kubehenge/cluster-api-provider-proxmox:<capmox release>-<commit>` | The build of Cluster API's Proxmox VE provider (capmox), released only while the module uses one. It is built from the upstream commit `capmox_build` names in `management-cluster/versions.tf`, with the fixes in `scripts/capmox-patches`. `scripts/provider-image.sh` builds it reproducibly and refuses an image whose digest differs from the module's. Anyone can rebuild it and compare. |
| `ghcr.io/kubehenge/cluster-api-addon-provider-helm:<release>-<commit>` | The build of the Add-on Provider for Helm that `helm_addon_build` names, with the fixes in `scripts/helm-addon-patches`. It is built and released as capmox's is. |

The Talos providers are not part of a Kubehenge release. The management cluster installs them from the releases of Kubehenge's forks, [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos). Each fork signs and attests its own image and files.

## How it is built and signed

A pushed tag starts `.github/workflows/release.yml`. It calls `.github/workflows/release-build.yml` to build and sign the release, then checks and publishes it. The jobs run in this order, with `sign` and `sbom` side by side:

1. `gate` refuses to go on unless:
   - the tag is annotated, carries a signature GitHub verifies and matches the chart's `appVersion`
   - it names a commit of main with a verified signature
   - that commit passed every check main's ruleset requires in the merge queue's run on it, the end-to-end tests among them
   - `docs/changelog.md` has a section for the version and `pkg/release/release.pub` is there.
2. `test` runs every lint and unit test and the integration tier again. A vulnerability published since the merge, in a Go module or an action, stops the release.
3. `build` pushes the three images and builds the archive, `install.sh` and `index.json`. The index is the previous release's signed index, checked against the release keys, with this release added.
4. `sign` signs `index.json` into `index.json.sig` with the archive's `kubehenge` binary, which accepts only the keys it was built to trust. The release's only job that holds the key (the `release` environment's `RELEASE_KEY` secret), it checks out nothing and checks the archive against its checksum.
5. `sbom` writes the SPDX SBOMs of the archive and of each image with syft (`scripts/sbom.sh`, `scripts/syft.yaml`) and checks each lists what ships. For the archive that is everything the table above names, for an image its program's Go module and the base image's packages.
6. `attest` signs the images with Sigstore (keyless) and attests build provenance and the SBOMs ([below](#what-is-attested)). It alone holds the OpenID Connect token Sigstore signs against and runs no code but cosign (installed by its checksum) and the attest action.
7. `verify` checks everything from the files alone, as a user would, with `release-build.yml` at this tag as the builder. It checks the checksums, each file's provenance, the archive's SBOM, and each image's provenance, SBOM and signature.
8. `publish` creates the release with every file, publishes it as the latest and starts the website's workflow. Its notes are the version's changelog section, opened before 1.0.0 by the README's alpha warning. A draft left by a failed run is made again, but a published release is never rewritten.

This is SLSA Build Level 3 on GitHub Actions. The build runs in a reusable workflow on GitHub-hosted runners, used once. The certificate Sigstore issues therefore names `release-build.yml`, not whatever called it. A verifier that insists on it (`--signer-workflow`, `--certificate-identity`) insists on its exact steps at the tag, which no caller can change. No build cache another workflow's run could have written reaches the release.

### The index appliances read

An index expires two weeks after it is signed. Appliances refuse an expired one, so whoever serves an appliance an old index can hold back a fix for two weeks at most.

A published release never changes, so its own `index.json` runs out. Appliances read the index at `https://kubehenge.dev/releases/index.json` instead. `.github/workflows/website.yml` signs it again every Monday, after each release and on every change to the website.

Its `Release index` job downloads the latest release's index, archive and provenance. It checks the provenance names `release-build.yml` at that tag. With the archive's `kubehenge` binary it checks the index against the release keys, renews its expiry (`kubehenge release renew`) and signs it. It is the website's only job that holds the key (the `release-index` environment's `RELEASE_KEY` secret) and checks out nothing. When it fails, the website is not published and the index already there stays until it expires.

### What is attested

| Subject | Attestations | Stored |
| --- | --- | --- |
| The archive | build provenance (SLSA v1), SBOM (SPDX 2.3) | on the repository, and in `kubehenge_<version>.intoto.jsonl` |
| `kubehenge_<version>_SHA256SUMS`, `install.sh`, `index.json`, `index.json.sig`, the archive's SBOM | build provenance | on the repository, and in `kubehenge_<version>.intoto.jsonl` |
| Each of the three images | build provenance, SBOM, cosign signature | on the repository, and beside the image in ghcr.io |
| `index.json` | the release key's signature, `index.json.sig` | the release, and signed again on kubehenge.dev |

## Once, before the first release

1. Make the release key:

   ```sh
   kubehenge release keygen release.key
   ```

   It prints the public key. Commit it as `pkg/release/release.pub`. To replace the key later, see [Rotating the release key](#rotating-the-release-key).
2. In the repository's settings:
   - Create the environment `release`, deployable only from tags matching `v*`, with the secret `RELEASE_KEY` holding the contents of `release.key`.
   - Create the environment `release-index`, deployable only from `main`, with the same secret `RELEASE_KEY`.
   - Add a tag ruleset for `v*` that blocks updates and deletions, so a released tag can never be moved.
   - Turn on immutable releases (Settings, General, Releases). A published release's files and tag then never change, and GitHub attests the release itself.
   - Add the key you sign tags with to your GitHub account as a signing key, so GitHub verifies the tag's signature.
3. After the first release, make the `kubehenge`, `cluster-api-provider-proxmox` and `cluster-api-addon-provider-helm` packages on ghcr.io public.

## Each release

1. In a pull request, set the version in `charts/kubehenge/Chart.yaml` (`appVersion`, and raise `version`) and write its section in `docs/changelog.md` (`## [<version>] - <date>`).
2. Once it is merged and main's checks have finished, tag main's commit and push the tag:

   ```sh
   git tag -s v<version> -m "Kubehenge <version>"
   git push origin v<version>
   ```

   The run does the rest ([above](#how-it-is-built-and-signed)). Appliances offer the release within minutes.

## Rotating the release key

Every build trusts each key in `pkg/release/release.pub` (one per line) and takes an index any of them signed. The sign jobs sign with `RELEASE_KEY` and, when it is set, `RELEASE_KEY_NEXT`, a line of `index.json.sig` each. So the key changes without stranding an appliance:

1. Make the new key with `kubehenge release keygen` and add its public key as a second line of `release.pub`. Give both environments, `release` and `release-index`, the secret `RELEASE_KEY_NEXT` holding it. Release. The index then carries both signatures and the release trusts both keys.
2. Once the appliances you look after run a release that trusts the new key, make it `RELEASE_KEY` and delete `RELEASE_KEY_NEXT`. Remove the old key's line from `release.pub` and release again.

When the old key leaks, skip the wait. Remove it from `release.pub` and the secrets at once and release. Appliances whose release trusts only the old key then stop seeing upgrades. They move on with `install.sh --upgrade` ([upgrading.md](upgrading.md)), which checks the archive against the checksum the script carries.

## Checking a release

Anyone can check a published release on a machine with the GitHub CLI and cosign. Run the commands from the root of a checkout of the tag, whose `management-cluster/versions.tf` supplies the image references.

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

`gh release verify` checks GitHub's attestation of the published release, its tag and every file as they now stay. `make verify-providers`, from the same checkout, checks the Talos providers the tag pins the same way, against their forks' release workflows. Until the packages on ghcr.io are public, the image checks need a login (`docker login ghcr.io`). `index.json` should hold the previous releases unchanged and one new entry whose checksum matches `SHA256SUMS`.

How users check a release before installing it is in [install.md](install.md#verifying-a-release).
