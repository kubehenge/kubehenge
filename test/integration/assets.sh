#!/bin/sh
# Fetches what the integration tier runs against: envtest's kube-apiserver and etcd for this platform,
# and the CustomResourceDefinitions of Cluster API, of the Cluster API Operator and of the providers
# whose objects Kubehenge's charts and controllers write, from the releases the management cluster
# installs. Each download is checked
# against its SHA-256, and nothing is fetched again while the versions stay the same.
#   test/integration/assets.sh <directory>   ->  <directory>/bin, <directory>/crds
set -eu

dir=${1:?usage: $0 <directory>}
ENVTEST=v1.37.0
# Each file its own version and SHA-256, so that Renovate moves them together.
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
CLUSTER_API=v1.14.4
CLUSTER_API_SHA256=0030264eb0a0d90ebacc9da8f6bbb717bee32e45ffadc4a74784fb5ad7b9bb0b
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
CLUSTER_API_BOOTSTRAP=v1.14.4
CLUSTER_API_BOOTSTRAP_SHA256=cf805775f24d197581b06e938bcd0eb9a9ed49d675f67e0f689ce7b71e5e397c
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
CLUSTER_API_CONTROL_PLANE=v1.14.4
CLUSTER_API_CONTROL_PLANE_SHA256=7fecb41843dcff7e05ab539dbe497c050a9aa7c888d4fb68be12d3c1805be438
# renovate: datasource=github-release-attachments depName=ionos-cloud/cluster-api-provider-proxmox
CAPMOX=v0.9.2
CAPMOX_SHA256=6817322435b42cfda07a8f94006bacb709afb83445ce24545af253c7904755d8
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-addon-provider-helm
HELM_ADDON=v0.6.4
HELM_ADDON_SHA256=a70f4eb77eac626231daca1e2a046b4b069bb84320efa327cc8c56a9c4ca03e6
# renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
RKE2_BOOTSTRAP=v0.27.2
RKE2_BOOTSTRAP_SHA256=0ba4c73bfe335bfa1b8a277b4df5f35e9a475f47f93038809010a1dc111063a2
# renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
RKE2_CONTROL_PLANE=v0.27.2
RKE2_CONTROL_PLANE_SHA256=f47336d0d34e6c96797d633470c7566973ba5b86925791fa68cbf60add607378
# renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
K3S_BOOTSTRAP=v0.5.0
K3S_BOOTSTRAP_SHA256=ed311227fd133dbdfccaad643f0c68c12c06f9bfec75221d57dffcefc8936a26
# renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
K3S_CONTROL_PLANE=v0.5.0
K3S_CONTROL_PLANE_SHA256=9032b67bb1a2642f967c00763ca3706ee0eaea34e855e646f0fec452e838b624
# renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-bootstrap-provider-talos
TALOS_BOOTSTRAP=v0.7.0
TALOS_BOOTSTRAP_SHA256=0a46d57902fa63a9ad47d986f26596bd93c8a82b0ca6af8bc89753f9a65eabd8
# renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-control-plane-provider-talos
TALOS_CONTROL_PLANE=v0.6.0
TALOS_CONTROL_PLANE_SHA256=806f88d687dd87862004aee1a557d8a7974df7bdb6bfb28f1f44e7ec7f1a2092
# renovate: datasource=github-release-attachments depName=cert-manager/cert-manager
CERT_MANAGER=v1.21.3
CERT_MANAGER_SHA256=29810b2a84c8ee0731a79679ee1dfd7e93cd750eb0244542eec47fa8700f2749
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-operator
CAPI_OPERATOR=v0.30.0
CAPI_OPERATOR_SHA256=6628cbbad5ae283a032fbe4f9ff94635038a0f09d7f6679b748e22010863f733

goos=$(go env GOOS)
goarch=$(go env GOARCH)
platform=${goos}-${goarch}
case "${platform}" in
    linux-amd64) envtest_sha256=f03faedfc68d2a78cee5cb02618bb44e7f67044f4c5fc957f72d3307fda61a9c ;;
    windows-amd64) envtest_sha256=755ad60aec5d99e4ea86363c2595b80144a4dceb5502de3b5c314cd923c29de9 ;;
    *)
        echo "integration-assets: no envtest checksum for ${platform}" >&2
        exit 1
        ;;
esac

stamp="${ENVTEST} ${platform} ${CLUSTER_API_SHA256} ${CLUSTER_API_BOOTSTRAP_SHA256} ${CLUSTER_API_CONTROL_PLANE_SHA256} ${CAPMOX_SHA256} ${HELM_ADDON_SHA256} ${RKE2_BOOTSTRAP_SHA256} ${RKE2_CONTROL_PLANE_SHA256} ${K3S_BOOTSTRAP_SHA256} ${K3S_CONTROL_PLANE_SHA256} ${TALOS_BOOTSTRAP_SHA256} ${TALOS_CONTROL_PLANE_SHA256} ${CERT_MANAGER_SHA256} ${CAPI_OPERATOR_SHA256}"
# No stamp yet means nothing is installed.
installed=$(cat "${dir}/versions" 2> /dev/null || true)
[ "${installed}" = "${stamp}" ] && exit 0

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fetch() { # url sha256 destination
    "$(dirname "$0")/../../scripts/fetch.sh" "$1" "${work}/download" || {
        echo "integration-assets: could not download $1" >&2
        exit 1
    }
    echo "$2  ${work}/download" | sha256sum -c --quiet -
    mv "${work}/download" "$3"
}

rm -rf "${dir:?}/bin" "${dir:?}/crds"
mkdir -p "${dir}/bin" "${dir}/crds"
fetch "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-${ENVTEST}/envtest-${ENVTEST}-${platform}.tar.gz" \
    "${envtest_sha256}" "${work}/envtest.tar.gz"
tar -xzf "${work}/envtest.tar.gz" -C "${work}"
cp "${work}"/controller-tools/envtest/* "${dir}/bin/"

# The components files hold more than CRDs; envtest installs only the CRDs among them.
gh=https://github.com
fetch "${gh}/kubernetes-sigs/cluster-api/releases/download/${CLUSTER_API}/core-components.yaml" \
    "${CLUSTER_API_SHA256}" "${dir}/crds/cluster-api.yaml"
fetch "${gh}/kubernetes-sigs/cluster-api/releases/download/${CLUSTER_API_BOOTSTRAP}/bootstrap-components.yaml" \
    "${CLUSTER_API_BOOTSTRAP_SHA256}" "${dir}/crds/kubeadm-bootstrap.yaml"
fetch "${gh}/kubernetes-sigs/cluster-api/releases/download/${CLUSTER_API_CONTROL_PLANE}/control-plane-components.yaml" \
    "${CLUSTER_API_CONTROL_PLANE_SHA256}" "${dir}/crds/kubeadm-control-plane.yaml"
fetch "${gh}/ionos-cloud/cluster-api-provider-proxmox/releases/download/${CAPMOX}/infrastructure-components.yaml" \
    "${CAPMOX_SHA256}" "${dir}/crds/proxmox.yaml"
fetch "${gh}/kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/${HELM_ADDON}/addon-components.yaml" \
    "${HELM_ADDON_SHA256}" "${dir}/crds/helm-addon.yaml"
fetch "${gh}/rancher/cluster-api-provider-rke2/releases/download/${RKE2_BOOTSTRAP}/bootstrap-components.yaml" \
    "${RKE2_BOOTSTRAP_SHA256}" "${dir}/crds/rke2-bootstrap.yaml"
fetch "${gh}/rancher/cluster-api-provider-rke2/releases/download/${RKE2_CONTROL_PLANE}/control-plane-components.yaml" \
    "${RKE2_CONTROL_PLANE_SHA256}" "${dir}/crds/rke2-control-plane.yaml"
fetch "${gh}/k3s-io/cluster-api-k3s/releases/download/${K3S_BOOTSTRAP}/bootstrap-components.yaml" \
    "${K3S_BOOTSTRAP_SHA256}" "${dir}/crds/k3s-bootstrap.yaml"
fetch "${gh}/k3s-io/cluster-api-k3s/releases/download/${K3S_CONTROL_PLANE}/control-plane-components.yaml" \
    "${K3S_CONTROL_PLANE_SHA256}" "${dir}/crds/k3s-control-plane.yaml"
fetch "${gh}/kubehenge/cluster-api-bootstrap-provider-talos/releases/download/${TALOS_BOOTSTRAP}/bootstrap-components.yaml" \
    "${TALOS_BOOTSTRAP_SHA256}" "${dir}/crds/talos-bootstrap.yaml"
fetch "${gh}/kubehenge/cluster-api-control-plane-provider-talos/releases/download/${TALOS_CONTROL_PLANE}/control-plane-components.yaml" \
    "${TALOS_CONTROL_PLANE_SHA256}" "${dir}/crds/talos-control-plane.yaml"
fetch "${gh}/cert-manager/cert-manager/releases/download/${CERT_MANAGER}/cert-manager.crds.yaml" \
    "${CERT_MANAGER_SHA256}" "${dir}/crds/cert-manager.yaml"
fetch "${gh}/kubernetes-sigs/cluster-api-operator/releases/download/${CAPI_OPERATOR}/operator-components.yaml" \
    "${CAPI_OPERATOR_SHA256}" "${dir}/crds/cluster-api-operator.yaml"
echo "${stamp}" > "${dir}/versions"
