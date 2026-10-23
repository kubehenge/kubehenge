#!/bin/sh
# Builds the image of a Cluster API provider the management cluster runs as Kubehenge builds it
# (<provider>_build in management-cluster/versions.tf, with the provider's dashes as underscores)
# from its upstream commit with the fixes in scripts/<provider>-patches, pushes it to <repository>
# and prints <repository>@<digest>. The build is reproducible: it fails unless the image comes out
# with the digest versions.tf names, so what a release publishes is what anyone gets from that commit
# and those patches. Further arguments go to ko, such as --insecure-registry.
#
# With --manifests, it renders instead the manifests of a provider whose block names their digests
# (`manifests`) from the same source, with kustomize and the image versions.tf names, into
# <directory>: config/default as control-plane-components.yaml and config/metadata/metadata.yaml.
# It fails unless both come out with the digests named there.
#
#   scripts/provider-image.sh <provider> <repository> [ko flags]
#   scripts/provider-image.sh --manifests <provider> <directory>
set -eu

usage="usage: $0 <provider> <repository> [ko flags] | --manifests <provider> <directory>"
manifests=
if [ "${1:-}" = --manifests ]; then
    manifests=${3:?${usage}}
fi
[ -z "${manifests}" ] || shift
provider=${1:?${usage}}
repository=${2:?${usage}}
shift 2
scripts=$(cd "$(dirname "$0")" && pwd)
versions=${scripts}/../management-cluster/versions.tf
block=$(echo "${provider}" | tr - _)_build
field() {
    sed -n "/${block} = {/,/^  }/s/^ *$1 *= *\"\(.*\)\"/\1/p" "${versions}"
}
upstream=$(field upstream)
package=$(field package)
release=$(field release)
commit=$(field commit)
go=$(field go)
base=$(field base_image)
image=$(field image)
if [ -z "${upstream}" ] || [ -z "${package}" ] || [ -z "${release}" ] || [ -z "${commit}" ] || [ -z "${go}" ] ||
    [ -z "${base}" ] || [ -z "${image}" ]; then
    echo "provider-image: ${block} in ${versions} is missing or incomplete" >&2
    exit 1
fi

src=$(mktemp -d)
archive=$(mktemp)
trap 'rm -rf "$src" "$archive"' EXIT
# No checksum for the archive: the digest check below covers the source, as any change to it changes
# the binary. It goes to a file, which curl can start again on a retry, where a pipe could not.
curl -fsSL --proto '=https' --retry 5 --retry-all-errors -o "${archive}" "https://github.com/${upstream}/archive/${commit}.tar.gz"
tar -xzf "${archive}" -C "${src}" --strip-components=1
for fix in "${scripts}/${provider}-patches"/*.patch; do
    [ -e "${fix}" ] || continue
    patch -d "${src}" -p1 --batch --forward --quiet < "${fix}"
done

# check <file> <role>: the file has the digest the block's manifests name for the role; status
# turns 1 when it has not, so both files are checked and reported before the script fails.
check() {
    want=$(sed -n "/${block} = {/,/^  }/s/^ *$2 *= *\"\([0-9a-f]*\)\"/\1/p" "${versions}")
    got=$(sha256sum "$1")
    got=${got%% *}
    if [ "${got}" != "${want}" ]; then
        echo "provider-image: rendered $(basename "$1") as ${got}, but ${block} names ${want:-none}" >&2
        status=1
    fi
}
if [ -n "${manifests}" ]; then
    out=${repository}
    (cd "${src}/config/manager" && kustomize edit set image "controller=${image}")
    kustomize build "${src}/config/default" > "${src}/control-plane-components.yaml"
    status=0
    check "${src}/config/metadata/metadata.yaml" metadata
    check "${src}/control-plane-components.yaml" components
    mkdir -p "${out}"
    cp "${src}/config/metadata/metadata.yaml" "${src}/control-plane-components.yaml" "${out}/"
    exit "${status}"
fi

build() {
    (cd "${src}" && GOTOOLCHAIN=${go} GOFLAGS='-trimpath -buildvcs=false' CGO_ENABLED=0 KO_DEFAULTBASEIMAGE=${base} \
        KO_DOCKER_REPO=${repository} ko build --bare --platform=linux/amd64 --sbom=none \
        --tags "${release}-$(printf %.12s "${commit}")" "$@" "${package}")
}
# Checked before anything is pushed; the second build comes from Go's cache.
digest=$(build --push=false "$@")
digest=${digest##*@}
if [ "${digest}" != "${image##*@}" ]; then
    echo "provider-image: built ${digest} from ${commit}, but ${block} names ${image##*@}" >&2
    exit 1
fi
ref=$(build "$@")
if [ "${ref##*@}" != "${digest}" ]; then
    echo "provider-image: pushed ${ref##*@}, not ${digest}" >&2
    exit 1
fi
echo "${repository}@${digest}"
