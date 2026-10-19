#!/bin/sh
# Builds the image of a Cluster API provider the management cluster runs as Kubehenge builds it
# (<provider>_build in management-cluster/versions.tf, with the provider's dashes as underscores)
# from its upstream commit with the fixes in scripts/<provider>-patches, pushes it to <repository>
# and prints <repository>@<digest>. The build is reproducible: it fails unless the image comes out
# with the digest versions.tf names, so what a release publishes is what anyone gets from that commit
# and those patches. Further arguments go to ko, such as --insecure-registry.
#
#   scripts/provider-image.sh <provider> <repository> [ko flags]
set -eu

provider=${1:?usage: $0 <provider> <repository> [ko flags]}
repository=${2:?usage: $0 <provider> <repository> [ko flags]}
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
trap 'rm -rf "$src"' EXIT
# No checksum for the archive: the digest check below covers the source, as any change to it changes
# the binary.
curl -fsSL --proto '=https' "https://github.com/${upstream}/archive/${commit}.tar.gz" |
    tar -xzf - -C "${src}" --strip-components=1
for fix in "${scripts}/${provider}-patches"/*.patch; do
    [ -e "${fix}" ] || continue
    patch -d "${src}" -p1 --batch --forward --quiet < "${fix}"
done

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
