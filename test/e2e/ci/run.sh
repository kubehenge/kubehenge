#!/bin/sh
# Runs one end-to-end test against the Proxmox VE test/e2e/ci/up.sh started, from the repository's
# root, with the controller and capmox built from the tree under test:
#
#   test/e2e/ci/run.sh <work directory> <test>
#   test/e2e/ci/run.sh <work directory> <test>,<test>     # a lane of two, one after the other
#
# <test> is management-cluster (applies the management cluster, runs the templates and volumes tests
# while it is up, and destroys it), management-cluster-upgrade (applies and destroys it), appliance
# (installs the appliance, which sets the management cluster up and removes it), appliance-upgrade
# (installs the latest published release and upgrades it to the tree under test), or kubeadm, rke2,
# k3s, talos, the halves of rke2 and k3s rke2-etcd, rke2-machines, k3s-etcd and k3s-machines, their
# add-on parts kubeadm-addons-a, -b and -c, rke2-addons-a to -d, k3s-addons-a and -b and
# talos-addons-a, -b and -c, rancher,
# cluster-mesh, node-image-refresh, autoscaler, control-plane-ha or two-nodes (which first joins a
# second Proxmox VE node, test/e2e/ci/second-node.sh), which first apply the management cluster and
# create the templates they need, and leave it up. The workload tests and their add-on halves take
# options after their name, each after a +: the cluster's CNI, its node OS, or cis-off
# (test/e2e/ci/variant.sh), as in kubeadm+calico+rocky-10+cis-off. Every test but the appliance's two
# takes management-<MiB>, the management node's memory, as in management-cluster-upgrade+management-3584.
#
# The workload clusters start one minor version behind the management cluster's Kubernetes and are
# upgraded to it; KUBEHENGE_E2E_FROM names the release they start on. The Talos cluster starts on
# the Talos release KUBEHENGE_E2E_TALOS_FROM names, by default the one before the release
# templates are made from, and its nodes are upgraded in place to the latter.
set -eu

work=$(readlink -f "${1:?usage: $0 <work directory> <test>}")
test=${2:?usage: $0 <work directory> <test>}
# lane_clear: no workload cluster and no VM of one is left, so the lane's next test has the runner's
# memory to itself. The management cluster and the templates stay for it.
lane_clear() {
    if [ -s "${work}/kubeconfig.yaml" ] && kubectl --kubeconfig "${work}/kubeconfig.yaml" get --raw /readyz > /dev/null 2>&1; then
        lane_left=$(kubectl --kubeconfig "${work}/kubeconfig.yaml" get clusters.cluster.x-k8s.io -A \
            -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {end}')
        if [ -n "${lane_left}" ]; then
            echo "the lane's next test would start beside the clusters ${lane_left}" >&2
            return 1
        fi
    fi
    lane_left=$(docker exec kubehenge-e2e-pve pvesh get /cluster/resources --type vm --output-format json |
        jq -j '.[] | select((.template // 0) == 0 and (.name | startswith("kubehenge-") | not)) | "\(.name) "')
    if [ -n "${lane_left}" ]; then
        echo "the lane's next test would start beside the VMs ${lane_left}" >&2
        return 1
    fi
}
# A lane of two tests (test/e2e/ci/scope.sh) runs each in turn on this Proxmox VE and stops at the
# first that fails, naming it in <work directory>/failed-test.
case "${test}" in
    *,*)
        lane=${test}
        lane_first=${lane%%,*}
        for lane_test in $(printf '%s' "${lane}" | tr , ' '); do
            lane_status=0
            # shellcheck disable=SC2310 # its refusal, which it explains, fails the test before it.
            if [ "${lane_test}" != "${lane_first}" ] && ! lane_clear; then
                lane_test=${lane_first}
                lane_status=1
            fi
            if [ "${lane_status}" = 0 ]; then
                echo "== ${lane_test}, in the lane ${lane}"
                KUBEHENGE_E2E_LANE=${lane} sh "$0" "${work}" "${lane_test}" || lane_status=$?
            fi
            if [ "${lane_status}" != 0 ]; then
                printf '%s\n' "${lane_test}" > "${work}/failed-test"
                echo "${lane_test} failed, in the lane ${lane}" >&2
                exit "${lane_status}"
            fi
        done
        exit 0
        ;;
    *) ;;
esac
# shellcheck source=variant.sh
. "$(dirname "$0")/variant.sh"
# shellcheck disable=SC2310 # its refusal, which it explains, ends the run.
settings=$(variant "${test}") || exit 2
eval "${settings}"
test=${test%%+*}
case "${test}" in
    management-cluster | management-cluster-upgrade | appliance | appliance-upgrade | kubeadm | rke2 | k3s | talos) ;;
    rke2-etcd | rke2-machines | k3s-etcd | k3s-machines) ;;
    kubeadm-addons-a | kubeadm-addons-b | kubeadm-addons-c) ;;
    rke2-addons-a | rke2-addons-b | rke2-addons-c | rke2-addons-d) ;;
    k3s-addons-a | k3s-addons-b | talos-addons-a | talos-addons-b | talos-addons-c) ;;
    rancher | cluster-mesh | node-image-refresh | autoscaler) ;;
    control-plane-ha | two-nodes) ;;
    *)
        echo "unknown test ${test}" >&2
        exit 2
        ;;
esac
repo=$(readlink -f "$(dirname "$0")/../../..")
# shellcheck source=/dev/null
. "${work}/env"
if [ "${test}" = two-nodes ]; then
    sh "${repo}/test/e2e/ci/second-node.sh" "${work}"
    # shellcheck source=/dev/null
    . "${work}/env"
    : "${KUBEHENGE_E2E_NODES:?}" "${KUBEHENGE_E2E_SHARED_STORAGE:?}"
    TF_VAR_proxmox_nodes=$(printf '%s' "${KUBEHENGE_E2E_NODES}" | jq -Rc 'split(",")')
    export TF_VAR_proxmox_nodes TF_VAR_datastore="${KUBEHENGE_E2E_SHARED_STORAGE}"
fi
: "${KUBEHENGE_PROXMOX_TOKEN:?}" "${KUBEHENGE_PROXMOX_CA:?}" "${KUBEHENGE_E2E_NODE:?}" "${KUBEHENGE_E2E_ADDRESS:?}"
: "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}" "${KUBEHENGE_E2E_REGISTRY:?}"

# shellcheck source=templates.sh
. "${repo}/test/e2e/ci/templates.sh"
kubernetes_versions "${repo}"
# What test/e2e/ci/build.sh built for the run, when it ran: the binary, and the images up.sh's
# registry already serves.
built=false
if [ -s "${work}/build/controller-digest" ]; then
    built=true
    mkdir -p "${repo}/dist"
    cp "${work}/build/kubehenge" "${repo}/dist/kubehenge"
    chmod +x "${repo}/dist/kubehenge"
fi
# pinned <rke2|k3s> <version>: the newest release of the version's minor that the controller pins;
# its nodes install no other.
pinned() {
    awk -v minor="${2%.*}." -v build="+$1" \
        'index($1, minor) == 1 && index($1, build) { print $1 }' "${repo}/pkg/provisioner/releasepins.txt" |
        sort -V | tail -n 1 | grep . || {
        echo "pkg/provisioner/releasepins.txt pins no $1 release of $2's minor version" >&2
        return 1
    }
}
rke2_from=$(pinned rke2 "${from}")
rke2_to=$(pinned rke2 "${to}")
k3s_from=$(pinned k3s "${from}")
k3s_to=$(pinned k3s "${to}")
if [ "${test%-addons-?}" = talos ]; then
    talos_to=$(sed -n 's/^\(const \)\{0,1\}[[:space:]]*DefaultTalosVersion = "\(v[^"]*\)".*/\2/p' "${repo}/pkg/templates/plan.go")
    [ -n "${talos_to}" ] || {
        echo "pkg/templates/plan.go names no DefaultTalosVersion" >&2
        exit 1
    }
    # The newest release before it that the Image Factory serves, the templates' source, which is
    # the patch before it unless it opens a minor version.
    talos_from=${KUBEHENGE_E2E_TALOS_FROM:-$(curl -fsS --retry 8 --retry-all-errors https://factory.talos.dev/versions |
        jq -r --arg to "${talos_to}" '
            def key: ltrimstr("v") | split(".") | map(tonumber);
            ($to | key) as $t | [.[] | select(test("^v[0-9]+[.][0-9]+[.][0-9]+$")) | select(key < $t)] | max_by(key) // empty')}
    [ -n "${talos_from}" ] || {
        echo "the Image Factory serves no Talos release before ${talos_to}" >&2
        exit 1
    }
fi

mkdir -p "${work}/mgmt" "${work}/plugins"
# The module installs the Cluster API operator from the chart archive beside it, which no checkout holds.
make -s -C "${repo}" capi-operator-chart
# OpenTofu reads only the root module's lock, so the module's own checks the providers.
cp "${repo}/test/e2e/ci/main.tf" "${repo}/management-cluster/.terraform.lock.hcl" "${work}/mgmt/"
export TF_PLUGIN_CACHE_DIR="${work}/plugins"
export TF_VAR_repository="${repo}" TF_VAR_proxmox_api_token="${KUBEHENGE_PROXMOX_TOKEN}"
TF_VAR_refresh_weekday=$(LC_ALL=C date -u -d '+3 days' +%a)
export TF_VAR_refresh_weekday
TF_VAR_proxmox_ca_certificate=$(cat "${KUBEHENGE_PROXMOX_CA}")
# OpenTofu's Proxmox VE provider trusts only the system's CAs and takes none of its own. Rather than
# change this machine's trust store, the run's processes get the system's CAs with Proxmox VE's
# beside them; Go reads SSL_CERT_FILE instead of the system bundle.
for bundle in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
    [ -f "${bundle}" ] && break
done
cat "${bundle}" "${KUBEHENGE_PROXMOX_CA}" > "${work}/ca-bundle.pem"
export SSL_CERT_FILE="${work}/ca-bundle.pem"

# The node OS a test's options chose, for the templates of its distribution.
node_os=
[ -z "${KUBEHENGE_E2E_NODE_OS:-}" ] || node_os="--node-os ${KUBEHENGE_E2E_NODE_OS}"
# templates.sh restores a template the Actions cache kept, or makes it (made).
# A template needs only Proxmox VE and the template pool, which the apply makes in its first seconds,
# so the ones a test needs are made while the management cluster comes up. One at a time: they share
# the build address.
templates=
# How many of them are kubeadm's, RKE2's or k3s's, each of whose builds proves, through Proxmox VE,
# that its guest agent hands out the node's boot log. A restored one proved it when it was baked:
# only a template that passed is kept.
case "${test}" in
    talos | talos-addons-?) boot_log_builds=0 ;;
    kubeadm) boot_log_builds=2 ;;
    *) boot_log_builds=1 ;;
esac
# node-image-refresh's test makes its Rocky Linux template, its second, only once the refresh's runs
# are done, which then build none of their own: each takes 8 to 9 minutes, and nothing checks them.
if [ "${test}" = node-image-refresh ]; then
    export KUBEHENGE_E2E_ROCKY_TEMPLATE="sh '${repo}/test/e2e/ci/templates.sh' made '${work}' 2 k3s --node-os rocky-10"
fi
case "${test}" in
    rke2* | k3s* | kubeadm* | talos* | rancher | cluster-mesh | node-image-refresh | autoscaler | control-plane-ha | two-nodes)
        [ "${built}" = true ] || make -s -C "${repo}" build
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
            # A template's slot is its place among the baked templates of the test's lane, as
            # templates.sh keys numbers their keys.
            lane_baked=$(baked "${KUBEHENGE_E2E_LANE:-${test}}")
            templates_of "${test}" > "${work}/templates.txt"
            while read -r options; do
                case "${options}" in
                    talos*) this=0 ;;
                    *) this=$(printf '%s\n' "${lane_baked}" | grep -nxF -- "${options}" | cut -d: -f1) ;;
                esac
                [ "${test}-${this}" != node-image-refresh-2 ] || continue
                # A lane's earlier test made it and checked its boot log.
                if grep -qxF -- "${options}" "${work}/templates-made" 2> /dev/null; then
                    echo "template ${options} made for the lane's earlier test"
                    continue
                fi
                # shellcheck disable=SC2086 # one option or argument per word
                made "${this}" ${options}
            done < "${work}/templates.txt"
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
    # grep -c exits non-zero when it counts none, which the comparison reports.
    checked=$(grep -c 'hands out its boot log and cloud-init.s output through the guest agent$' "${work}/templates.log" || true)
    restored=$(grep -c -e '^template .* restored from the cache as VM ' -e "^template .* made for the lane's earlier test$" "${work}/templates.log" || true)
    boot_log_builds=$((boot_log_builds - restored))
    if [ "${checked}" -lt "${boot_log_builds}" ]; then
        echo "${checked} of the ${boot_log_builds} node templates were seen handing out their boot logs" >&2
        exit 1
    fi
    sed 's/ *$//' "${work}/templates.txt" >> "${work}/templates-made"
}

if [ "${built}" = true ]; then
    TF_VAR_controller_image=ghcr.io/kubehenge/kubehenge@$(cat "${work}/build/controller-digest")
else
    TF_VAR_controller_image=ghcr.io/kubehenge/kubehenge@$(make -s -C "${repo}" image \
        IMAGE="${KUBEHENGE_E2E_REGISTRY}/kubehenge/kubehenge" KO_FLAGS=--insecure-registry | sed -n 's/.*@//p')
fi
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
if [ "${built}" != true ]; then
    make -s -C "${repo}" capmox-image CAPMOX_IMAGE="${KUBEHENGE_E2E_REGISTRY}/kubehenge/cluster-api-provider-proxmox" \
        KO_FLAGS=--insecure-registry > /dev/null
    make -s -C "${repo}" helm-addon-image HELM_ADDON_IMAGE="${KUBEHENGE_E2E_REGISTRY}/kubehenge/cluster-api-addon-provider-helm" \
        KO_FLAGS=--insecure-registry > /dev/null
fi

# The management node's memory is the module's default, but where the test's name gives another.
# kubeadm-addons-a and k3s-addons-b with Calico and rke2-addons-b with Cilium take 3584 MiB, as
# with 4096 their guests outgrow 12 GiB (docs/testing.md). two-nodes takes it too. Its two nodes
# share this machine's memory, and the second rejoins the placement only with a tenth more than the
# 4 GiB control plane free.
case "${test}+${KUBEHENGE_E2E_CNI:-}" in
    kubeadm-addons-a+calico | k3s-addons-b+calico | rke2-addons-b+cilium | two-nodes+*) : "${KUBEHENGE_E2E_MANAGEMENT_MEMORY:=3584}" ;;
    *) ;;
esac
if [ -n "${KUBEHENGE_E2E_MANAGEMENT_MEMORY:-}" ]; then
    export TF_VAR_node_memory_mb="${KUBEHENGE_E2E_MANAGEMENT_MEMORY}"
    echo "the management node has ${TF_VAR_node_memory_mb} MiB of memory"
fi
# The workload nodes of the tests in balloon_tests give memory back through their balloons too, down
# to what the sizing figures say they need (room.sh).
# shellcheck source=room.sh
. "${repo}/test/e2e/ci/room.sh"
case " ${balloon_tests} " in
    *" ${test} "*) export KUBEHENGE_E2E_BALLOON_FILE="${RUNNER_TEMP:-/tmp}/balloon.txt" ;;
    *) ;;
esac
# Talos keeps its page cache, so the management node gives memory back only through its balloon.
# It used up to 2752 MB with the add-on tests (docs/testing.md). At 3072 MiB it saw only 2874 MB
# and thrashed, so the minimum leaves it about 3350 MB.
export TF_VAR_node_memory_min_mb="${KUBEHENGE_E2E_MANAGEMENT_MEMORY_MIN:-3584}"

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
        # Three releases of the tree under test, to install the first and upgrade to the others,
        # through the appliance's own updater and through install.sh. They trust a release key
        # made for the run, never the published one, which signs the index the test serves the
        # updater. The test runs on the node, where the installer does; the appliance sets up the
        # management cluster as main.tf does.
        make -s -C "${repo}" build > /dev/null
        release_key=$("${repo}/dist/kubehenge" release keygen "${work}/release.key")
        for version in 0.0.1 0.0.2 0.0.3; do
            make -s -C "${repo}" dist VERSION="${version}" RELEASE_PUBLIC_KEYS="${release_key}" > /dev/null
        done
        jq -n --arg image "${TF_VAR_controller_image}" '{
            nodes: ["pve"], imageDatastore: "local", vmDatastore: "local", bootstrapDatastore: "local",
            bridge: "vmbr0", nodeAddresses: ["172.20.99.11/24"], apiAddress: "172.20.99.10",
            gateway: "172.20.99.1", dnsServers: ["172.20.99.1"],
            workloadDatastores: ["local"], workloadBridges: ["vmbr0"],
            registryMirrors: {"ghcr.io": ["http://172.21.99.3:5000"]}, controllerImage: $image,
            offerHARules: true, extraClusterNamespaces: ["e2e-team"], headlampSignIn: true,
            headlampActions: true, apiAuditLog: true
        }' > "${work}/appliance-settings.json"
        pve=kubehenge-e2e-pve
        docker exec "${pve}" sh -c 'rm -rf /root/kubehenge && mkdir -p /root/kubehenge/test'
        docker cp "${repo}/test/e2e" "${pve}:/root/kubehenge/test/e2e"
        docker cp "${repo}/appliance" "${pve}:/root/kubehenge/appliance"
        for version in 0.0.1 0.0.2 0.0.3; do
            docker cp "${repo}/dist/kubehenge_${version}_linux_amd64.tar.gz" "${pve}:/root/kubehenge/"
        done
        docker cp "${work}/appliance-settings.json" "${pve}:/root/kubehenge/settings.json"
        docker cp "${work}/release.key" "${pve}:/root/kubehenge/release.key"
        exec docker exec -e KUBEHENGE_E2E_APPLIANCE_ID=900 -e KUBEHENGE_E2E_APPLIANCE_ADDRESS=172.20.99.40/24 \
            -e KUBEHENGE_E2E_GATEWAY="${KUBEHENGE_E2E_GATEWAY}" -e KUBEHENGE_E2E_DNS="${KUBEHENGE_E2E_DNS}" \
            -e KUBEHENGE_E2E_STORAGE=local -e KUBEHENGE_E2E_S3_ADDRESS=172.20.99.1 \
            -e KUBEHENGE_E2E_RELEASE_KEY=/root/kubehenge/release.key \
            "${pve}" sh /root/kubehenge/test/e2e/appliance.sh \
            /root/kubehenge/kubehenge_0.0.1_linux_amd64.tar.gz /root/kubehenge/kubehenge_0.0.2_linux_amd64.tar.gz \
            /root/kubehenge/kubehenge_0.0.3_linux_amd64.tar.gz /root/kubehenge/settings.json
        ;;
    appliance-upgrade)
        # What a user does: the newest published release installed with the installer published
        # beside it, then the tree under test as the release after it. The tag is read when the run
        # starts, from the redirect of the releases page rather than the API, whose limit a shared
        # runner address may have used up. The published files are checked against the release's
        # SHA256SUMS, so no key of this run's is involved in installing them.
        tag=$(curl -sSL --proto '=https' --retry 8 --retry-all-errors -o /dev/null -w '%{url_effective}' \
            https://github.com/kubehenge/kubehenge/releases/latest)
        tag=${tag##*/}
        printf '%s' "${tag}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || {
            echo "the latest release is tagged ${tag}, not vX.Y.Z" >&2
            exit 1
        }
        published=${tag#v}
        mkdir "${work}/published"
        for asset in install.sh "kubehenge_${published}_linux_amd64.tar.gz" "kubehenge_${published}_SHA256SUMS"; do
            "${repo}/scripts/fetch.sh" "https://github.com/kubehenge/kubehenge/releases/download/${tag}/${asset}" \
                "${work}/published/${asset}"
        done
        (cd "${work}/published" && sha256sum -c --strict "kubehenge_${published}_SHA256SUMS") || {
            echo "the published release does not have the sums its SHA256SUMS names" >&2
            exit 1
        }
        # install.sh refuses an older release, so the build is the published version's next patch.
        next=${published%.*}.$((${published##*.} + 1))
        make -s -C "${repo}" build > /dev/null
        make -s -C "${repo}" dist VERSION="${next}" > /dev/null
        # The fields the published release's interface takes, which refuses one it does not know:
        # the network and storage ones every release's setup has asked for, and the registry
        # mirror, whose misses the nodes pass on to the registry. The controller image is the
        # published release's own, and the tree under test's goes in with the upgrade's apply.
        jq -n '{
            nodes: ["pve"], imageDatastore: "local", vmDatastore: "local", bootstrapDatastore: "local",
            bridge: "vmbr0", nodeAddresses: ["172.20.99.11/24"], apiAddress: "172.20.99.10",
            gateway: "172.20.99.1", dnsServers: ["172.20.99.1"],
            workloadDatastores: ["local"], workloadBridges: ["vmbr0"],
            registryMirrors: {"ghcr.io": ["http://172.21.99.3:5000"]}
        }' > "${work}/appliance-settings.json"
        pve=kubehenge-e2e-pve
        docker exec "${pve}" sh -c 'rm -rf /root/kubehenge && mkdir -p /root/kubehenge/test'
        docker cp "${repo}/test/e2e" "${pve}:/root/kubehenge/test/e2e"
        docker cp "${repo}/appliance" "${pve}:/root/kubehenge/appliance"
        docker cp "${work}/published/install.sh" "${pve}:/root/kubehenge/published-install.sh"
        docker cp "${work}/published/kubehenge_${published}_linux_amd64.tar.gz" "${pve}:/root/kubehenge/"
        docker cp "${repo}/dist/kubehenge_${next}_linux_amd64.tar.gz" "${pve}:/root/kubehenge/"
        docker cp "${work}/appliance-settings.json" "${pve}:/root/kubehenge/settings.json"
        exec docker exec -e KUBEHENGE_E2E_APPLIANCE_ID=900 -e KUBEHENGE_E2E_APPLIANCE_ADDRESS=172.20.99.40/24 \
            -e KUBEHENGE_E2E_GATEWAY="${KUBEHENGE_E2E_GATEWAY}" -e KUBEHENGE_E2E_DNS="${KUBEHENGE_E2E_DNS}" \
            -e KUBEHENGE_E2E_STORAGE=local -e KUBEHENGE_E2E_CONTROLLER_IMAGE="${TF_VAR_controller_image}" \
            "${pve}" sh /root/kubehenge/test/e2e/appliance-upgrade.sh /root/kubehenge/published-install.sh \
            "/root/kubehenge/kubehenge_${published}_linux_amd64.tar.gz" "/root/kubehenge/kubehenge_${next}_linux_amd64.tar.gz" \
            /root/kubehenge/settings.json
        ;;
    # The other tests run against a management cluster applied here first. The runner has 4 vCPUs,
    # and a workload cluster with every add-on starting starves when the management node holds 4 of
    # its own beside it; with no provider upgrade to ride out, 2 do. Its memory is set above: with
    # 3 GiB the node stalled for half a minute while the add-on provider reconciled every release
    # at once, its probes failing, and the webhooks with them.
    *) export TF_VAR_node_cores=2 ;;
esac

# The workload cluster and Cluster Mesh tests back up to an S3 store they serve on this machine, at
# the address the nodes reach it by, under a certificate of a CA made for the run; the management
# cluster offers it as a backup target, and as a second one, which nothing backs up to, for the
# clusters' reads of each other's backups. A lane's later test keeps the store and its certificate
# when an earlier one set them up, so that the management cluster's apply changes nothing.
s3=${work}/s3
case "${test}" in
    rke2* | k3s* | kubeadm* | talos* | cluster-mesh) serves_s3=true ;;
    *) serves_s3=false ;;
esac
[ ! -s "${s3}/tls.crt" ] || serves_s3=true
case "${serves_s3}" in
    true)
        mkdir -p "${s3}"
        if [ ! -s "${s3}/tls.crt" ]; then
            openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=kubehenge-e2e-s3-ca \
                -keyout "${s3}/ca.key" -out "${s3}/ca.crt" 2> /dev/null
            openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=kubehenge-e2e-s3 \
                -keyout "${s3}/tls.key" -out "${s3}/tls.csr" 2> /dev/null
            printf 'subjectAltName = IP:%s\nextendedKeyUsage = serverAuth\n' "${KUBEHENGE_E2E_OIDC_ADDRESS:?}" > "${s3}/san.cnf"
            openssl x509 -req -in "${s3}/tls.csr" -CA "${s3}/ca.crt" -CAkey "${s3}/ca.key" -CAcreateserial \
                -CAserial "${s3}/ca.srl" -days 2 -extfile "${s3}/san.cnf" -out "${s3}/tls.crt" 2> /dev/null
        fi
        TF_VAR_backup_targets=$(jq -cn --arg e "https://${KUBEHENGE_E2E_OIDC_ADDRESS}:8333" --rawfile ca "${s3}/ca.crt" \
            '[{name: "e2e", endpoint: $e, bucket: "kubehenge-e2e", prefix: "ci", ca_certificate: $ca},
              {name: "e2e-other", endpoint: $e, bucket: "kubehenge-e2e-other", prefix: "ci", ca_certificate: $ca}]')
        export TF_VAR_backup_targets KUBEHENGE_E2E_BACKUP_TARGET=e2e KUBEHENGE_E2E_BACKUP_OTHER_TARGET=e2e-other
        export KUBEHENGE_E2E_BACKUP_CA="${s3}/ca.crt"
        export KUBEHENGE_E2E_BACKUP_CERT="${s3}/tls.crt" KUBEHENGE_E2E_BACKUP_KEY="${s3}/tls.key"
        ;;
    *) ;;
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
# For the guard, which samples the management node's memory from inside it once this is here.
(umask 077 && tofu -chdir="${work}/mgmt" output -raw talosconfig > "${work}/mgmt/talosconfig")

templates_made

# Nested vCPUs cost no memory, and with two an RKE2 control plane stopped answering its own lease
# renewals while everything started.
export KUBEHENGE_E2E_CONTROL_PLANE_CORES=4
# Each class's workload test runs in parts, on a machine each, as the whole outgrows one machine's
# memory: kubeadm, rke2, k3s and talos take a cluster of the least size the class allows with the
# add-ons it starts with through everything but the other add-ons, and their -addons-a, -addons-b,
# -addons-c but on k3s and -addons-d on RKE2 parts one sized for a group of the add-ons through that
# group, replacing no machine. rke2's and k3s's own test also runs in two halves, -etcd and
# -machines, on a machine each.
case "${test}" in
    *-addons-?) export KUBEHENGE_E2E_PART="addons-${test##*-}" ;;
    *-etcd | *-machines) export KUBEHENGE_E2E_PART="${test#*-}" ;;
    rke2 | k3s | kubeadm | talos) export KUBEHENGE_E2E_PART=lifecycle ;;
    *) ;;
esac
case "${test}" in
    rke2 | rke2-*)
        # MetalLB is the default the other distributions run; kube-vip gets a cluster of its own.
        export KUBEHENGE_E2E_LOAD_BALANCER=kube-vip
        # The one cluster that checks its nodes' HA group labels, which every cluster has by default,
        # and switches them off and on again.
        case "${test}" in
            rke2 | rke2-etcd) export KUBEHENGE_E2E_HA_GROUPS=1 ;;
            *) ;;
        esac
        e2e e2e-workload-cluster CLASS=kubehenge-rke2 VERSION="${rke2_from}" UPGRADE="${rke2_to}"
        ;;
    k3s | k3s-*)
        # The one cluster on the storage of its own, whose machines are cloned there. Its etcd half
        # switches the CSI plugin's newest release on and off, and volume snapshots, with the copy
        # proxy's image pulled from KUBEHENGE_E2E_REGISTRY, where this build was pushed.
        case "${test}" in
            k3s | k3s-etcd | k3s-machines) export KUBEHENGE_E2E_STORAGE=own ;;
            *) ;;
        esac
        case "${test}" in
            k3s | k3s-etcd)
                export KUBEHENGE_E2E_DATASTORE_ALLOCATE=1 KUBEHENGE_E2E_LOCAL_STORAGE=local
                export KUBEHENGE_E2E_VOLUME_SNAPSHOTS=1
                ;;
            *) ;;
        esac
        # In one of main.tf's extra_cluster_namespaces, created with the interface's own account.
        export KUBEHENGE_E2E_NAMESPACE=e2e-team
        e2e e2e-workload-cluster CLASS=kubehenge-k3s VERSION="${k3s_from}" UPGRADE="${k3s_to}"
        ;;
    kubeadm | kubeadm-addons-?)
        export KUBEHENGE_E2E_CNI="${KUBEHENGE_E2E_CNI:-cilium}"
        e2e e2e-workload-cluster CLASS=kubehenge-kubeadm VERSION="${from}" UPGRADE="${to}"
        ;;
    talos | talos-addons-?)
        # Talos's own CNI, flannel, unless an option chose another: the kubeadm run covers Cilium.
        export KUBEHENGE_E2E_TALOS_VERSION="${talos_from}" KUBEHENGE_E2E_TALOS_UPGRADE="${talos_to}"
        e2e e2e-workload-cluster CLASS=kubehenge-talos VERSION="${from}" UPGRADE="${to}"
        ;;
    rancher)
        export KUBEHENGE_E2E_RANCHER_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_RANCHER_ADDRESSES=172.20.99.21-172.20.99.24
        export KUBEHENGE_E2E_DOWNSTREAM_ENDPOINT=172.20.99.25 KUBEHENGE_E2E_DOWNSTREAM_ADDRESSES=172.20.99.26-172.20.99.29
        export KUBEHENGE_E2E_RANCHER_VERSION="${k3s_from}" KUBEHENGE_E2E_DOWNSTREAM_VERSION="${k3s_from}"
        export KUBEHENGE_E2E_BLOCKED_VERSION="${k3s_to}"
        e2e e2e-rancher
        ;;
    cluster-mesh)
        export KUBEHENGE_E2E_EAST_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_EAST_ADDRESSES=172.20.99.21-172.20.99.24
        export KUBEHENGE_E2E_WEST_ENDPOINT=172.20.99.25 KUBEHENGE_E2E_WEST_ADDRESSES=172.20.99.26-172.20.99.29
        export KUBEHENGE_E2E_VERSION="${k3s_from}"
        e2e e2e-cluster-mesh
        ;;
    node-image-refresh)
        export KUBEHENGE_E2E_VERSION="${k3s_from}"
        e2e e2e-node-image-refresh
        ;;
    autoscaler)
        export KUBEHENGE_E2E_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_ADDRESSES=172.20.99.21-172.20.99.24
        export KUBEHENGE_E2E_VERSION="${k3s_from}"
        e2e e2e-autoscaler
        ;;
    control-plane-ha)
        export KUBEHENGE_E2E_VERSION="${from}+k3s1"
        e2e e2e-control-plane-ha
        ;;
    two-nodes)
        export KUBEHENGE_E2E_VERSION="${from}+k3s1"
        # pve2 leaves the cluster's membership with its services stopped, VMs and network kept, and
        # pve alone keeps the quorum it would lose with one vote of two.
        export KUBEHENGE_E2E_DOWN='docker exec kubehenge-e2e-pve2 systemctl stop pve-ha-lrm pve-ha-crm pvestatd pveproxy pvedaemon pve-cluster corosync && docker exec kubehenge-e2e-pve pvecm expected 1'
        export KUBEHENGE_E2E_UP='docker exec kubehenge-e2e-pve2 systemctl start corosync pve-cluster pvedaemon pveproxy pvestatd'
        e2e e2e-two-nodes
        ;;
    # Checked before anything was built.
    *) ;;
esac
