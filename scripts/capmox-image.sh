#!/bin/sh
# Builds the capmox controller image the management cluster runs (capmox_build in
# management-cluster/versions.tf) from its upstream commit with the fixes in scripts/capmox-patches,
# pushes it to <repository> and prints <repository>@<digest>. The build is reproducible: it fails
# unless the image comes out with the digest versions.tf names, so what a release publishes is what
# anyone gets from that commit and those patches. Further arguments go to ko, such as
# --insecure-registry.
#
#   scripts/capmox-image.sh <repository> [ko flags]
set -eu

repository=${1:?usage: $0 <repository> [ko flags]}
shift
scripts=$(cd "$(dirname "$0")" && pwd)
versions=${scripts}/../management-cluster/versions.tf
field() {
    sed -n "/capmox_build = {/,/^  }/s/^ *$1 *= *\"\(.*\)\"/\1/p" "${versions}"
}
release=$(field release)
commit=$(field commit)
go=$(field go)
base=$(field base_image)
image=$(field image)
if [ -z "${release}" ] || [ -z "${commit}" ] || [ -z "${go}" ] || [ -z "${base}" ] || [ -z "${image}" ]; then
    echo "capmox-image: capmox_build in ${versions} is incomplete" >&2
    exit 1
fi

src=$(mktemp -d)
trap 'rm -rf "$src"' EXIT
# No checksum for the archive: the digest check below covers the source, as any change to it changes
# the binary.
curl -fsSL --proto '=https' "https://github.com/ionos-cloud/cluster-api-provider-proxmox/archive/${commit}.tar.gz" |
    tar -xzf - -C "${src}" --strip-components=1
for fix in "${scripts}"/capmox-patches/*.patch; do
    [ -e "${fix}" ] || continue
    patch -d "${src}" -p1 --batch --forward --quiet < "${fix}"
done

build() {
    (cd "${src}" && GOTOOLCHAIN=${go} GOFLAGS='-trimpath -buildvcs=false' CGO_ENABLED=0 KO_DEFAULTBASEIMAGE=${base} \
        KO_DOCKER_REPO=${repository} ko build --bare --platform=linux/amd64 --sbom=none \
        --tags "${release}-$(printf %.12s "${commit}")" "$@" ./cmd)
}
# Checked before anything is pushed; the second build comes from Go's cache.
digest=$(build --push=false "$@")
digest=${digest##*@}
if [ "${digest}" != "${image##*@}" ]; then
    echo "capmox-image: built ${digest} from ${commit}, but versions.tf names ${image##*@}" >&2
    exit 1
fi
ref=$(build "$@")
if [ "${ref##*@}" != "${digest}" ]; then
    echo "capmox-image: pushed ${ref##*@}, not ${digest}" >&2
    exit 1
fi
echo "${repository}@${digest}"
