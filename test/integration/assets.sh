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
CLUSTER_API=v1.14.3
CAPMOX=v0.9.1
HELM_ADDON=v0.6.4
RKE2=v0.26.0
K3S=v0.4.0
TALOS_BOOTSTRAP=v0.7.0-alpha.3
CERT_MANAGER=v1.21.2
CAPI_OPERATOR=v0.29.0
# No release publishes the Talos control-plane provider's manifests; Kubehenge's binary carries them.
talos_control_plane=$(cd "$(dirname "$0")/../.." && pwd)/pkg/provisioner/providers/talos-control-plane/control-plane-components.yaml
TALOS_CONTROL_PLANE=$(sha256sum "${talos_control_plane}")
TALOS_CONTROL_PLANE=${TALOS_CONTROL_PLANE%% *}

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

stamp="${ENVTEST} ${platform} ${CLUSTER_API} ${CAPMOX} ${HELM_ADDON} ${RKE2} ${K3S} ${TALOS_BOOTSTRAP} ${TALOS_CONTROL_PLANE} ${CERT_MANAGER} ${CAPI_OPERATOR}"
# No stamp yet means nothing is installed.
installed=$(cat "${dir}/versions" 2> /dev/null || true)
[ "${installed}" = "${stamp}" ] && exit 0

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fetch() { # url sha256 destination
    curl -fsSL --proto '=https' --retry 5 --retry-all-errors --tlsv1.2 -o "${work}/download" "$1"
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
capi=https://github.com/kubernetes-sigs/cluster-api/releases/download/${CLUSTER_API}
fetch "${capi}/core-components.yaml" f70be959af4aba061b91cb264b3b1961ae38ca49e2eb542ccf2640146f5221a7 "${dir}/crds/cluster-api.yaml"
fetch "${capi}/bootstrap-components.yaml" 537d6db3b70aad723710825f76073659865dc1a115715ef5d5c470da3b9bcc39 "${dir}/crds/kubeadm-bootstrap.yaml"
fetch "${capi}/control-plane-components.yaml" a211e2e54b6b6fb93e59191f5db8ee76ad6ff92a698f1c62f9d6ce4d5218cf87 "${dir}/crds/kubeadm-control-plane.yaml"
fetch "https://github.com/ionos-cloud/cluster-api-provider-proxmox/releases/download/${CAPMOX}/infrastructure-components.yaml" \
    424fb38189659147df452490bb2b11f9f4ae3a75c7179b92a85041658bfd74d5 "${dir}/crds/proxmox.yaml"
fetch "https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/${HELM_ADDON}/addon-components.yaml" \
    a70f4eb77eac626231daca1e2a046b4b069bb84320efa327cc8c56a9c4ca03e6 "${dir}/crds/helm-addon.yaml"
rke2=https://github.com/rancher/cluster-api-provider-rke2/releases/download/${RKE2}
fetch "${rke2}/bootstrap-components.yaml" 0356080e7071833ac3931f657e0dbd41b2b2ad7c0699575bf2fed19ed5a45f3e "${dir}/crds/rke2-bootstrap.yaml"
fetch "${rke2}/control-plane-components.yaml" 1dbed3911fa2f6c208020cae74e2d31f6fba6366ad639aa8666a03a75c80443a "${dir}/crds/rke2-control-plane.yaml"
k3s=https://github.com/k3s-io/cluster-api-k3s/releases/download/${K3S}
fetch "${k3s}/bootstrap-components.yaml" 26301df3f6a1b9a14317061bd586a73711d54c956ee8f679982fb3301c6ab71d "${dir}/crds/k3s-bootstrap.yaml"
fetch "${k3s}/control-plane-components.yaml" 2e3754bb42be197c31b7567496767f22091f4da74453eb366d29c8bd06e28270 "${dir}/crds/k3s-control-plane.yaml"
fetch "https://github.com/siderolabs/cluster-api-bootstrap-provider-talos/releases/download/${TALOS_BOOTSTRAP}/bootstrap-components.yaml" \
    3e2e7b785aef2b71f6241a02b539cd720b889de77be6ef6f4a90e180549dd78b "${dir}/crds/talos-bootstrap.yaml"
cp "${talos_control_plane}" "${dir}/crds/talos-control-plane.yaml"
fetch "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER}/cert-manager.crds.yaml" \
    262fef78478492cd35b73a1b227106b7802f9c056361d1f561d04b32d751337f "${dir}/crds/cert-manager.yaml"
fetch "https://github.com/kubernetes-sigs/cluster-api-operator/releases/download/${CAPI_OPERATOR}/operator-components.yaml" \
    4af5a3ebbd4e83ab78c84344c62ea85ea72b6866de28a5409cabcc2fd744bb6c "${dir}/crds/cluster-api-operator.yaml"
echo "${stamp}" > "${dir}/versions"
