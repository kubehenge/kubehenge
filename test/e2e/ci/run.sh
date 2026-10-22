#!/bin/sh
# Runs one end-to-end test against the Proxmox VE test/e2e/ci/up.sh started, from the repository's
# root, with the controller and capmox built from the tree under test:
#
#   test/e2e/ci/run.sh <work directory> <test>
#
# <test> is management-cluster (applies the management cluster, runs the templates and volumes tests
# while it is up, and destroys it), management-cluster-upgrade (applies and destroys it), appliance
# (installs the appliance, which sets the management cluster up and removes it), or kubeadm, rke2,
# k3s, rancher or cluster-mesh, which first apply the management cluster and create the templates
# they need, and leave it up.
#
# The workload clusters start one minor version behind the management cluster's Kubernetes and are
# upgraded to it; KUBEHENGE_E2E_FROM names the release they start on.
set -eu

work=$(readlink -f "${1:?usage: $0 <work directory> <test>}")
test=${2:?usage: $0 <work directory> <test>}
case "${test}" in
    management-cluster | management-cluster-upgrade | appliance | kubeadm | rke2 | k3s | rancher | cluster-mesh) ;;
    *)
        echo "unknown test ${test}" >&2
        exit 2
        ;;
esac
repo=$(readlink -f "$(dirname "$0")/../../..")
# shellcheck source=/dev/null
. "${work}/env"
: "${KUBEHENGE_PROXMOX_TOKEN:?}" "${KUBEHENGE_PROXMOX_CA:?}" "${KUBEHENGE_E2E_NODE:?}" "${KUBEHENGE_E2E_ADDRESS:?}"
: "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_REGISTRY:?}"

from=${KUBEHENGE_E2E_FROM:-v1.36.5}
to=$(sed -n 's/^ *kubernetes *= *"\(v[^"]*\)".*/\1/p' "${repo}/management-cluster/versions.tf")
[ -n "${to}" ]

mkdir -p "${work}/mgmt" "${work}/plugins"
# OpenTofu reads only the root module's lock, so the module's own checks the providers.
cp "${repo}/test/e2e/ci/main.tf" "${repo}/management-cluster/.terraform.lock.hcl" "${work}/mgmt/"
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

# The host's CPU: told of a generic one, the guests' kernels run Meltdown's and MDS's mitigations,
# which under nested virtualisation slow them on Intel runners until a worker loses its API server.
template() { # distribution [flags]
    "${repo}/dist/kubehenge" template create "$@" --cpu-type host --node "${KUBEHENGE_E2E_NODE}" --storage local \
        --build-address "${KUBEHENGE_E2E_ADDRESS}" --build-gateway "${KUBEHENGE_E2E_GATEWAY}" \
        --build-dns "${KUBEHENGE_E2E_DNS}"
}
# A template needs only Proxmox VE and the template pool, which the apply makes in its first seconds,
# so the ones a test needs are made while the management cluster comes up. One at a time: they share
# the build address.
templates=
case "${test}" in
    rke2 | k3s | kubeadm | rancher | cluster-mesh)
        make -s -C "${repo}" build
        {
            waited=0
            until docker exec kubehenge-e2e-pve pvesh get /pools/kubehenge-templates > /dev/null 2>&1; do
                if [ "${waited}" -ge 1200 ]; then
                    echo "the apply made no pool kubehenge-templates in 20 minutes"
                    exit 1
                fi
                sleep 5
                waited=$((waited + 5))
            done
            case "${test}" in
                rke2) template rke2 ;;
                k3s | rancher | cluster-mesh) template k3s ;;
                kubeadm)
                    template kubeadm --kubernetes-version "${from}"
                    template kubeadm --kubernetes-version "${to}"
                    ;;
                *) ;;
            esac
        } > "${work}/templates.log" 2>&1 &
        templates=$!
        ;;
    *) ;;
esac
templates_made() {
    [ -n "${templates}" ] || return 0
    # shellcheck disable=SC2310 # the status is reported here, with the log of what failed.
    if ! wait "${templates}"; then
        cat "${work}/templates.log" >&2
        echo "the templates were not made" >&2
        exit 1
    fi
    cat "${work}/templates.log"
}

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
# capmox and the Add-on Provider for Helm as a release publishes them, by the digests the module
# names; the mirror serves them as ghcr.io's.
make -s -C "${repo}" capmox-image CAPMOX_IMAGE="${KUBEHENGE_E2E_REGISTRY}/kubehenge/cluster-api-provider-proxmox" \
    KO_FLAGS=--insecure-registry > /dev/null
make -s -C "${repo}" helm-addon-image HELM_ADDON_IMAGE="${KUBEHENGE_E2E_REGISTRY}/kubehenge/cluster-api-addon-provider-helm" \
    KO_FLAGS=--insecure-registry > /dev/null

# The tests' own output is shown as it comes rather than when each make target ends.
e2e() { make -C "${repo}" OUTPUT_SYNC=none "$@"; }

case "${test}" in
    management-cluster)
        # The templates test needs the pool the module makes, and the volumes test the controller's
        # API token, so both run while the management cluster is up.
        export KUBEHENGE_E2E_POOL=kubehenge-templates KUBEHENGE_E2E_LOCAL_STORAGE=local
        exec sh "${repo}/test/e2e/management-cluster.sh" "${work}/mgmt" sh -ec '
            KUBEHENGE_E2E_CONTROLLER_TOKEN=$(kubectl -n kubehenge-system get secret kubehenge-proxmox \
                -o jsonpath="{.data.token}" | base64 -d)
            export KUBEHENGE_E2E_CONTROLLER_TOKEN
            make -C "$1" OUTPUT_SYNC=none e2e-templates
            make -C "$1" OUTPUT_SYNC=none e2e-volumes' sh "${repo}"
        ;;
    management-cluster-upgrade)
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
            gateway: "172.20.99.1", dnsServers: ["172.20.99.1"],
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
    # The other tests run against a management cluster applied here first. The runner has 4 vCPUs,
    # and a workload cluster with every add-on starting starves when the management node holds 4 of
    # its own beside it; with no provider upgrade to ride out, 2 do.
    *) export TF_VAR_node_cores=2 ;;
esac

tofu -chdir="${work}/mgmt" init -input=false -lockfile=readonly > /dev/null
if ! tofu -chdir="${work}/mgmt" apply -input=false -auto-approve -no-color > "${work}/apply.log"; then
    tail -n 50 "${work}/apply.log" >&2
    # A failed apply can still leave the management cluster up, and the job's artifacts then come
    # from it: its pods, events and controller logs.
    (umask 077 && tofu -chdir="${work}/mgmt" output -raw kubeconfig > "${work}/kubeconfig.yaml") ||
        rm -f "${work}/kubeconfig.yaml"
    exit 1
fi
(umask 077 && tofu -chdir="${work}/mgmt" output -raw kubeconfig > "${work}/kubeconfig.yaml")
export KUBECONFIG="${work}/kubeconfig.yaml"

templates_made

# Nested vCPUs cost no memory, and with two an RKE2 control plane stopped answering its own lease
# renewals while everything started.
export KUBEHENGE_E2E_CONTROL_PLANE_CORES=4
# The control planes' least memory, so that every add-on is seen to fit in it; the workers get
# theirs with the add-ons from the test itself.
export KUBEHENGE_E2E_CONTROL_PLANE_MEMORY=4096
case "${test}" in
    rke2)
        # MetalLB is the default the other distributions run; kube-vip gets a cluster of its own.
        export KUBEHENGE_E2E_LOAD_BALANCER=kube-vip
        e2e e2e-workload-cluster CLASS=kubehenge-rke2 VERSION="${from}+rke2r1" UPGRADE="${to}+rke2r1"
        ;;
    k3s)
        # The one cluster on the storage of its own, which switches the CSI plugin's newest release
        # on and off.
        export KUBEHENGE_E2E_STORAGE=own KUBEHENGE_E2E_DATASTORE_ALLOCATE=1 KUBEHENGE_E2E_LOCAL_STORAGE=local
        e2e e2e-workload-cluster CLASS=kubehenge-k3s VERSION="${from}+k3s1" UPGRADE="${to}+k3s1"
        ;;
    kubeadm)
        export KUBEHENGE_E2E_CNI="${KUBEHENGE_E2E_CNI:-cilium}"
        e2e e2e-workload-cluster CLASS=kubehenge-kubeadm VERSION="${from}" UPGRADE="${to}"
        ;;
    rancher)
        export KUBEHENGE_E2E_RANCHER_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_RANCHER_ADDRESSES=172.20.99.21-172.20.99.24
        export KUBEHENGE_E2E_DOWNSTREAM_ENDPOINT=172.20.99.25 KUBEHENGE_E2E_DOWNSTREAM_ADDRESSES=172.20.99.26-172.20.99.29
        export KUBEHENGE_E2E_RANCHER_VERSION="${from}+k3s1" KUBEHENGE_E2E_DOWNSTREAM_VERSION="${from}+k3s1"
        export KUBEHENGE_E2E_BLOCKED_VERSION="${to}+k3s1"
        e2e e2e-rancher
        ;;
    cluster-mesh)
        export KUBEHENGE_E2E_EAST_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_EAST_ADDRESSES=172.20.99.21-172.20.99.24
        export KUBEHENGE_E2E_WEST_ENDPOINT=172.20.99.25 KUBEHENGE_E2E_WEST_ADDRESSES=172.20.99.26-172.20.99.29
        export KUBEHENGE_E2E_VERSION="${from}+k3s1"
        e2e e2e-cluster-mesh
        ;;
    # Checked before anything was built.
    *) ;;
esac
