#!/bin/sh
# Brings the files a dependency update leaves stale in step with it:
#
#   scripts/renovate-refresh.sh
#
# - The images the Tigera operator runs Calico from, and the operator itself, go to the release the
#   tigera-operator chart in charts/kubehenge/values.yaml carries, each by its digest. Calico pushes
#   a release's images days before it publishes the release and its chart, so they never move alone.
# - The AlmaLinux checksum file and its signature the template tests serve, which have to list the
#   build pkg/templates/almalinux.go names. They come from the folder of that build's minor release.
#
# make release-pins refreshes the Kubernetes and Talos release pins beside this.
set -eu

values=charts/kubehenge/values.yaml
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

curl_get() {
    curl -fsSL --proto '=https' --retry 8 --retry-all-errors --tlsv1.2 "$@"
}

# digest <registry> <repository> <tag>: the digest the registry serves the tag's index or manifest by.
digest() {
    curl_get -o /dev/null -D - \
        -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
        "https://$1/v2/$2/manifests/$3" | tr -d '\r' | tr '[:upper:]' '[:lower:]' | sed -n 's/^docker-content-digest: *//p'
}

# The chart's release, from the version line below `chart: tigera-operator`.
release=$(sed -n '/^ *chart: tigera-operator$/{n;s/^ *version: *\([^@]*\)@.*/\1/p;}' "${values}")
case "${release}" in
    v[0-9]*) ;;
    *)
        echo "renovate-refresh: no tigera-operator chart version in ${values}" >&2
        exit 1
        ;;
esac

# The operator release the chart's own values name, from the archive values.yaml pins.
chart_sha256=$(sed -n '/^ *chart: tigera-operator$/{n;s/^ *version: *[^@]*@sha256:\([0-9a-f]*\)$/\1/p;}' "${values}")
scripts/fetch.sh "https://github.com/projectcalico/calico/releases/download/${release}/tigera-operator-${release}.tgz" "${work}/chart.tgz"
echo "${chart_sha256}  ${work}/chart.tgz" | sha256sum -c --quiet -
helm show values "${work}/chart.tgz" > "${work}/chart-values.yaml"
operator=$(sed -n '/^tigeraOperator:/,/^[^ ]/s/^ *version: *//p' "${work}/chart-values.yaml" | head -n 1)
case "${operator}" in
    v[0-9]*) ;;
    *)
        echo "renovate-refresh: tigera-operator ${release} names no operator version" >&2
        exit 1
        ;;
esac

cp "${values}" "${work}/values.yaml"
operator_digest=$(digest quay.io tigera/operator "${operator}")
[ -n "${operator_digest}" ] || {
    echo "renovate-refresh: quay.io/tigera/operator:${operator} has no digest" >&2
    exit 1
}
sed -i "/depName=quay.io\/tigera\/operator$/{n;s|version: .*|version: ${operator}@${operator_digest}|;}" "${work}/values.yaml"
sed -n 's|^ *- quay\.io/calico/\([a-z0-9-]*\):.*|\1|p' "${values}" > "${work}/calico-images"
while read -r name; do
    image_digest=$(digest quay.io "calico/${name}" "${release}")
    [ -n "${image_digest}" ] || {
        echo "renovate-refresh: quay.io/calico/${name}:${release} has no digest" >&2
        exit 1
    }
    sed -i "s|^\( *- quay\.io/calico/${name}\):.*|\1:${release}@${image_digest}|" "${work}/values.yaml"
done < "${work}/calico-images"
cmp -s "${work}/values.yaml" "${values}" || cp "${work}/values.yaml" "${values}"

alma=pkg/templates/testdata/almalinux-10
build=$(sed -n 's/^[[:space:]]*Build: *"\(.*\)",$/\1/p' pkg/templates/almalinux.go)
image="AlmaLinux-10-GenericCloud-${build}.x86_64_v2.qcow2"
if ! grep -qF "${image}" "${alma}/CHECKSUM"; then
    for file in CHECKSUM CHECKSUM.asc; do
        curl_get -o "${work}/${file}" "https://repo.almalinux.org/almalinux/${build%%-*}/cloud/x86_64_v2/images/${file}"
    done
    grep -qF "${image}" "${work}/CHECKSUM" || {
        echo "renovate-refresh: AlmaLinux's CHECKSUM of ${build%%-*} does not list build ${build}" >&2
        exit 1
    }
    cp "${work}/CHECKSUM" "${work}/CHECKSUM.asc" "${alma}/"
fi
