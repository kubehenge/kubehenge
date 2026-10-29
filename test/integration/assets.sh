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
CLUSTER_API=v1.14.3
CLUSTER_API_SHA256=f70be959af4aba061b91cb264b3b1961ae38ca49e2eb542ccf2640146f5221a7
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
CLUSTER_API_BOOTSTRAP=v1.14.3
CLUSTER_API_BOOTSTRAP_SHA256=537d6db3b70aad723710825f76073659865dc1a115715ef5d5c470da3b9bcc39
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
CLUSTER_API_CONTROL_PLANE=v1.14.3
CLUSTER_API_CONTROL_PLANE_SHA256=a211e2e54b6b6fb93e59191f5db8ee76ad6ff92a698f1c62f9d6ce4d5218cf87
# renovate: datasource=github-release-attachments depName=ionos-cloud/cluster-api-provider-proxmox
CAPMOX=v0.9.1
CAPMOX_SHA256=424fb38189659147df452490bb2b11f9f4ae3a75c7179b92a85041658bfd74d5
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-addon-provider-helm
HELM_ADDON=v0.6.4
HELM_ADDON_SHA256=a70f4eb77eac626231daca1e2a046b4b069bb84320efa327cc8c56a9c4ca03e6
# renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
RKE2_BOOTSTRAP=v0.26.0
RKE2_BOOTSTRAP_SHA256=0356080e7071833ac3931f657e0dbd41b2b2ad7c0699575bf2fed19ed5a45f3e
# renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
RKE2_CONTROL_PLANE=v0.26.0
RKE2_CONTROL_PLANE_SHA256=1dbed3911fa2f6c208020cae74e2d31f6fba6366ad639aa8666a03a75c80443a
# renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
K3S_BOOTSTRAP=v0.4.0
K3S_BOOTSTRAP_SHA256=26301df3f6a1b9a14317061bd586a73711d54c956ee8f679982fb3301c6ab71d
# renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
K3S_CONTROL_PLANE=v0.4.0
K3S_CONTROL_PLANE_SHA256=2e3754bb42be197c31b7567496767f22091f4da74453eb366d29c8bd06e28270
# renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-bootstrap-provider-talos
TALOS_BOOTSTRAP=v0.7.0
TALOS_BOOTSTRAP_SHA256=0a46d57902fa63a9ad47d986f26596bd93c8a82b0ca6af8bc89753f9a65eabd8
# renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-control-plane-provider-talos
TALOS_CONTROL_PLANE=v0.6.0
TALOS_CONTROL_PLANE_SHA256=806f88d687dd87862004aee1a557d8a7974df7bdb6bfb28f1f44e7ec7f1a2092
# renovate: datasource=github-release-attachments depName=cert-manager/cert-manager
CERT_MANAGER=v1.21.2
CERT_MANAGER_SHA256=262fef78478492cd35b73a1b227106b7802f9c056361d1f561d04b32d751337f
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
    curl -fsSL --proto '=https' --retry 5 --retry-all-errors --tlsv1.2 -o "${work}/download" "$1" || {
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
