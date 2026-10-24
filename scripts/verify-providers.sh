#!/bin/sh
# Checks the Cluster API providers the management cluster runs from Kubehenge's own forks (those
# provider_repositories in management-cluster/versions.tf names under kubehenge/) the way a user
# checks Kubehenge's own release: each file the module installs the provider from at its tested
# version has the SHA-256 versions.tf names and the build provenance of the fork's release workflow
# at that version's tag; its components name the image versions.tf pins; and that image carries the
# same provenance and a keyless cosign signature of the same workflow. A pin that moves to anything
# else fails here before it is merged. Needs curl, the GitHub CLI (GH_TOKEN in CI) and cosign.
#
#   scripts/verify-providers.sh
set -eu

versions=$(cd "$(dirname "$0")/.." && pwd)/management-cluster/versions.tf
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

fail() {
    echo "verify-providers: $*" >&2
    exit 1
}

# block <local>: the lines of the map versions.tf assigns to <local>.
block() {
    sed -n "/^  $1 = {/,/^  }/p" "${versions}"
}

forks=$(block provider_repositories | sed -n 's|^ *\([a-z0-9_]*\) *= *"kubehenge/\([^"]*\)".*|\1 \2|p')
[ -n "${forks}" ] || fail "${versions} names no provider from Kubehenge's forks"

while read -r component repository; do
    version=$(block tested_versions | sed -n "s/^ *${component} *= *\"\([^\"]*\)\".*/\1/p")
    [ -n "${version}" ] || fail "no tested version of ${component}"
    workflow=kubehenge/${repository}/.github/workflows/release.yaml
    attested() {
        gh attestation verify "$1" --repo "kubehenge/${repository}" --signer-workflow "${workflow}" \
            --source-ref "refs/tags/${version}" --deny-self-hosted-runners > /dev/null ||
            fail "$1 carries no provenance of ${workflow} at ${version}"
    }

    files=$(block tested_manifests |
        sed -n "s|^ *\"kubehenge/${repository}/releases/download/\([^/\"]*\)/\([^/\"]*\)\" *= *\"\([0-9a-f]\{64\}\)\".*|\1 \2 \3|p")
    count=$(printf '%s\n' "${files}" | grep -c .) || count=0
    [ "${count}" -eq 2 ] || fail "tested_manifests names no metadata and components of kubehenge/${repository}"
    components=
    while read -r release file digest; do
        [ "${release}" = "${version}" ] || fail "${file} of kubehenge/${repository} is pinned at ${release}, the tested version at ${version}"
        mkdir -p "${work}/${repository}"
        curl -fsSL --proto '=https' --retry 5 --retry-all-errors -o "${work}/${repository}/${file}" \
            "https://github.com/kubehenge/${repository}/releases/download/${release}/${file}"
        echo "${digest}  ${work}/${repository}/${file}" | sha256sum --check --strict --quiet ||
            fail "${file} of kubehenge/${repository} ${release} does not have the SHA-256 versions.tf names"
        attested "${work}/${repository}/${file}"
        case ${file} in
            *-components.yaml) components=${work}/${repository}/${file} ;;
            *) ;;
        esac
    done << EOF
${files}
EOF
    [ -n "${components}" ] || fail "tested_manifests names no components of kubehenge/${repository}"

    image=$(block tested_provider_images | sed -n "s|^ *\"[^\"]*\" *= *\"\(ghcr.io/kubehenge/${repository}:[^\"]*\)\".*|\1|p")
    count=$(printf '%s\n' "${image}" | grep -c .) || count=0
    [ "${count}" -eq 1 ] || fail "tested_provider_images names not exactly one image of kubehenge/${repository}"
    tag=${image#*:}
    tag=${tag%%@*}
    digest=${image##*@}
    [ "${tag}" = "${version}" ] || fail "${image} is not the tested version ${version}"
    grep -q "^ *image: ghcr.io/kubehenge/${repository}[:@].*${digest}\$" "${components}" ||
        fail "the components of kubehenge/${repository} ${version} do not name ${image}"
    attested "oci://ghcr.io/kubehenge/${repository}@${digest}"
    cosign verify "ghcr.io/kubehenge/${repository}@${digest}" \
        --certificate-identity "https://github.com/${workflow}@refs/tags/${version}" \
        --certificate-oidc-issuer https://token.actions.githubusercontent.com > "${work}/cosign.log" 2>&1 || {
        cat "${work}/cosign.log" >&2
        fail "${image} carries no cosign signature of ${workflow} at ${version}"
    }
    echo "kubehenge/${repository} ${version}: files, image and signatures verified"
done << EOF
${forks}
EOF
