#!/bin/sh
# Runs one end-to-end test against the Proxmox VE test/e2e/ci/up.sh started, from the repository's
# root, with the controller and capmox built from the tree under test:
#
#   test/e2e/ci/run.sh <work directory> <test>
#
# <test> is management-cluster or management-cluster-upgrade (each applies and destroys the
# management cluster), appliance (installs the appliance, which sets the management cluster up and
# removes it), or templates, kubeadm, rke2, k3s, volumes or rancher, which first apply the
# management cluster and create the templates they need, and leave it up.
#
# The workload clusters start one minor version behind the management cluster's Kubernetes and are
# upgraded to it; KUBEHENGE_E2E_FROM names the release they start on.
set -eu

work=$(readlink -f "${1:?usage: $0 <work directory> <test>}")
test=${2:?usage: $0 <work directory> <test>}
repo=$(readlink -f "$(dirname "$0")/../../..")
# shellcheck source=/dev/null
. "${work}/env"
: "${KUBEHENGE_PROXMOX_TOKEN:?}" "${KUBEHENGE_PROXMOX_CA:?}" "${KUBEHENGE_E2E_NODE:?}" "${KUBEHENGE_E2E_ADDRESS:?}"
: "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_REGISTRY:?}"

from=${KUBEHENGE_E2E_FROM:-v1.36.5}
to=$(sed -n 's/^ *kubernetes *= *"\(v[^"]*\)".*/\1/p' "${repo}/management-cluster/versions.tf")
[ -n "${to}" ]

mkdir -p "${work}/mgmt" "${work}/plugins"
cp "${repo}/test/e2e/ci/main.tf" "${work}/mgmt/"
export TF_PLUGIN_CACHE_DIR="${work}/plugins"
export TF_VAR_repository="${repo}" TF_VAR_proxmox_api_token="${KUBEHENGE_PROXMOX_TOKEN}"
TF_VAR_proxmox_ca_certificate=$(cat "${KUBEHENGE_PROXMOX_CA}")
# OpenTofu's Proxmox VE provider trusts only the system's CAs and takes none of its own. Rather than
# change this machine's trust store, the run's processes get the system's CAs with Proxmox VE's
# beside them; Go reads SSL_CERT_FILE instead of the system bundle.
for bundle in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
    [ -f "${bundle}" ] && break
done
cat "${bundle}" "${KUBEHENGE_PROXMOX_CA}" > "${work}/ca-bundle.pem"
export SSL_CERT_FILE="${work}/ca-bundle.pem"
TF_VAR_controller_image=ghcr.io/kubehenge/kubehenge@$(make -s -C "${repo}" image \
    IMAGE="${KUBEHENGE_E2E_REGISTRY}/kubehenge/kubehenge" KO_FLAGS=--insecure-registry | sed -n 's/.*@//p')
export TF_VAR_proxmox_ca_certificate TF_VAR_controller_image
case "${TF_VAR_controller_image}" in
    *@)
        echo "the controller image was not pushed" >&2
        exit 1
        ;;
    # Anything else ends in the pushed image's digest.
    *) ;;
esac
# capmox as a release publishes it, by the digest the module names; the mirror serves it as ghcr.io's.
make -s -C "${repo}" capmox-image CAPMOX_IMAGE="${KUBEHENGE_E2E_REGISTRY}/kubehenge/cluster-api-provider-proxmox" \
    KO_FLAGS=--insecure-registry > /dev/null

case "${test}" in
    management-cluster | management-cluster-upgrade)
        exec sh "${repo}/test/e2e/${test}.sh" "${work}/mgmt"
        ;;
    appliance)
        # Two releases of the tree under test, to install the one and upgrade to the other. The
        # test runs on the node, where the installer does; the appliance sets up the management
        # cluster as main.tf does.
        make -s -C "${repo}" dist VERSION=0.0.1 > /dev/null
        make -s -C "${repo}" dist VERSION=0.0.2 > /dev/null
        jq -n --arg image "${TF_VAR_controller_image}" '{
            nodes: ["pve"], imageDatastore: "local", vmDatastore: "local", bootstrapDatastore: "local",
            bridge: "vmbr0", nodeAddresses: ["172.20.99.11/24"], apiAddress: "172.20.99.10",
            gateway: "172.20.99.1", dnsServers: ["1.1.1.1", "9.9.9.9"],
            workloadDatastores: ["local"], workloadBridges: ["vmbr0"],
            registryMirrors: {"ghcr.io": ["http://172.21.99.3:5000"]}, controllerImage: $image
        }' > "${work}/appliance-settings.json"
        pve=kubehenge-e2e-pve
        docker exec "${pve}" sh -c 'rm -rf /root/kubehenge && mkdir -p /root/kubehenge/test'
        docker cp "${repo}/test/e2e" "${pve}:/root/kubehenge/test/e2e"
        docker cp "${repo}/appliance" "${pve}:/root/kubehenge/appliance"
        for version in 0.0.1 0.0.2; do
            docker cp "${repo}/dist/kubehenge_${version}_linux_amd64.tar.gz" "${pve}:/root/kubehenge/"
        done
        docker cp "${work}/appliance-settings.json" "${pve}:/root/kubehenge/settings.json"
        exec docker exec -e KUBEHENGE_E2E_APPLIANCE_ID=900 -e KUBEHENGE_E2E_APPLIANCE_ADDRESS=172.20.99.40/24 \
            -e KUBEHENGE_E2E_GATEWAY="${KUBEHENGE_E2E_GATEWAY}" -e KUBEHENGE_E2E_DNS="${KUBEHENGE_E2E_DNS}" \
            -e KUBEHENGE_E2E_STORAGE=local "${pve}" sh /root/kubehenge/test/e2e/appliance.sh \
            /root/kubehenge/kubehenge_0.0.1_linux_amd64.tar.gz /root/kubehenge/kubehenge_0.0.2_linux_amd64.tar.gz \
            /root/kubehenge/settings.json
        ;;
    # The other tests run against a management cluster applied here first.
    *) ;;
esac

tofu -chdir="${work}/mgmt" init -input=false > /dev/null
if ! tofu -chdir="${work}/mgmt" apply -input=false -auto-approve -no-color > "${work}/apply.log"; then
    tail -n 50 "${work}/apply.log" >&2
    exit 1
fi
(umask 077 && tofu -chdir="${work}/mgmt" output -raw kubeconfig > "${work}/kubeconfig.yaml")
export KUBECONFIG="${work}/kubeconfig.yaml"

make -s -C "${repo}" build
template() { # distribution [flags]
    "${repo}/bin/kubehenge" template create "$@" --node "${KUBEHENGE_E2E_NODE}" --storage local \
        --build-address "${KUBEHENGE_E2E_ADDRESS}" --build-gateway "${KUBEHENGE_E2E_GATEWAY}" \
        --build-dns "${KUBEHENGE_E2E_DNS}"
}

# Nested vCPUs cost no memory, and with two an RKE2 control plane stopped answering its own lease
# renewals while everything started.
export KUBEHENGE_E2E_CONTROL_PLANE_CORES=4
export KUBEHENGE_E2E_CONTROL_PLANE_MEMORY=2048 KUBEHENGE_E2E_WORKER_MEMORY=1024
cd "${repo}"
case "${test}" in
    templates)
        export KUBEHENGE_E2E_POOL=kubehenge-templates
        exec make e2e-templates
        ;;
    rke2)
        template rke2
        # RKE2's static pods request 2 GiB between them; the class's minimum.
        export KUBEHENGE_E2E_CONTROL_PLANE_MEMORY=3072
        exec make e2e-workload-cluster CLASS=kubehenge-rke2 VERSION="${from}+rke2r1" UPGRADE="${to}+rke2r1"
        ;;
    k3s)
        template k3s
        exec make e2e-workload-cluster CLASS=kubehenge-k3s VERSION="${from}+k3s1" UPGRADE="${to}+k3s1"
        ;;
    kubeadm)
        template kubeadm --kubernetes-version "${from}"
        template kubeadm --kubernetes-version "${to}"
        export KUBEHENGE_E2E_CNI="${KUBEHENGE_E2E_CNI:-cilium}"
        exec make e2e-workload-cluster CLASS=kubehenge-kubeadm VERSION="${from}" UPGRADE="${to}"
        ;;
    volumes)
        KUBEHENGE_E2E_CONTROLLER_TOKEN=$(kubectl -n kubehenge-system get secret kubehenge-proxmox \
            -o jsonpath='{.data.token}' | base64 -d)
        export KUBEHENGE_E2E_CONTROLLER_TOKEN KUBEHENGE_E2E_LOCAL_STORAGE=local
        exec make e2e-volumes
        ;;
    rancher)
        template rke2
        export KUBEHENGE_E2E_RANCHER_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_RANCHER_ADDRESSES=172.20.99.21-172.20.99.24
        export KUBEHENGE_E2E_DOWNSTREAM_ENDPOINT=172.20.99.25 KUBEHENGE_E2E_DOWNSTREAM_ADDRESSES=172.20.99.26-172.20.99.29
        export KUBEHENGE_E2E_RANCHER_VERSION="${from}+rke2r1" KUBEHENGE_E2E_DOWNSTREAM_VERSION="${from}+k3s1"
        export KUBEHENGE_E2E_BLOCKED_VERSION="${to}+rke2r1"
        export KUBEHENGE_E2E_RANCHER_MEMORY=6144 KUBEHENGE_E2E_DOWNSTREAM_MEMORY=2048
        exec make e2e-rancher
        ;;
    *)
        echo "unknown test ${test}" >&2
        exit 2
        ;;
esac
