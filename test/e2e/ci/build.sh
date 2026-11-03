#!/bin/sh
# Builds once, for every end-to-end lane of a run, what run.sh would otherwise build in each: the
# kubehenge binary, and the images of the controller, capmox and the Add-on Provider for Helm in a
# registry's storage, which up.sh then serves from the lane's own registry under the same digests.
# capmox's and the Add-on Provider's are checked against management-cluster/versions.tf as
# scripts/provider-image.sh always checks them.
#
#   test/e2e/ci/build.sh <out directory>
#
# Writes <out>/kubehenge, <out>/registry and <out>/controller-digest; run.sh uses them once they are
# in <work directory>/build.
set -eu

out=${1:?usage: $0 <out directory>}
script=$(readlink -f "$0")
here=$(dirname "${script}")
repo=$(readlink -f "${here}/../../..")
# The registry up.sh runs, pinned there.
image=$(sed -n 's/^REGISTRY_IMAGE=//p' "${here}/up.sh")
[ -n "${image}" ]

mkdir -p "${out}/registry"
out=$(readlink -f "${out}")
docker run -d --name kubehenge-e2e-build-registry -p 127.0.0.1:5000:5000 \
    -v "${out}/registry:/var/lib/registry" -e OTEL_TRACES_EXPORTER=none "${image}" > /dev/null
trap 'docker rm -f kubehenge-e2e-build-registry > /dev/null' EXIT
registry=127.0.0.1:5000

# The providers' builds spend most of their time fetching their source and Go toolchain, so they run
# beside the controller's.
make -s -C "${repo}" capmox-image CAPMOX_IMAGE="${registry}/kubehenge/cluster-api-provider-proxmox" \
    KO_FLAGS=--insecure-registry > "${out}/capmox.log" 2>&1 &
capmox=$!
make -s -C "${repo}" helm-addon-image HELM_ADDON_IMAGE="${registry}/kubehenge/cluster-api-addon-provider-helm" \
    KO_FLAGS=--insecure-registry > "${out}/helm-addon.log" 2>&1 &
helm_addon=$!
make -s -C "${repo}" build
cp "${repo}/dist/kubehenge" "${out}/kubehenge"
digest=$(make -s -C "${repo}" image IMAGE="${registry}/kubehenge/kubehenge" KO_FLAGS=--insecure-registry |
    sed -n 's/.*@//p')
case "${digest}" in
    sha256:*) ;;
    *)
        echo "the controller image was not pushed" >&2
        exit 1
        ;;
esac
for provider in capmox helm-addon; do
    if [ "${provider}" = capmox ]; then pid=${capmox}; else pid=${helm_addon}; fi
    # shellcheck disable=SC2310 # the status is reported here, with the build's log.
    if ! wait "${pid}"; then
        cat "${out}/${provider}.log" >&2
        echo "the ${provider} image was not built" >&2
        exit 1
    fi
    tail -n 1 "${out}/${provider}.log"
    rm "${out}/${provider}.log"
done
printf '%s\n' "${digest}" > "${out}/controller-digest"
echo "built kubehenge and the controller's image ${digest}"
