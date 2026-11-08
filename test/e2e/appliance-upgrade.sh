#!/bin/sh
# End-to-end test of a user's upgrade path on a Proxmox VE node: install the latest published release
# of the appliance with the installer it was published with, set the management cluster up from that
# release's interface, upgrade the appliance to the build under test with install.sh --upgrade, apply
# the management cluster's upgrade from the interface, and check that no management cluster VM was
# replaced and no network card re-created. Then remove the management cluster from the interface and
# the appliance, and check that Proxmox VE is left as it was.
#
#   test/e2e/appliance-upgrade.sh <published install.sh> <published release> <release> <settings>
#
# Runs as root on the node and needs jq. <published release> is the archive of the latest published
# release, whose sum the lane checked against the release's SHA256SUMS, and <published install.sh>
# the installer published beside it. <release> is the archive `make dist` built from the tree under
# test with a version above the published one. <settings> is a JSON file of the management cluster's
# settings as the published release's interface accepts them (pkg/appliance/settings.go of that
# release, whose interface refuses a field it does not know). KUBEHENGE_E2E_CONTROLLER_IMAGE is the
# controller image of the tree under test, which the upgrade's apply names because no release
# publishes it. The appliance is container KUBEHENGE_E2E_APPLIANCE_ID at
# KUBEHENGE_E2E_APPLIANCE_ADDRESS (with its prefix length) on KUBEHENGE_E2E_BRIDGE (default vmbr0),
# with the gateway KUBEHENGE_E2E_GATEWAY and the name servers KUBEHENGE_E2E_DNS. Its disk goes on
# KUBEHENGE_E2E_STORAGE when that is set.
#
# A failed run leaves the appliance and whatever it set up, to look at; remove them as
# docs/uninstalling.md says before running it again.
set -eu

usage="usage: $0 <published install.sh> <published release> <release> <settings>"
published_install=${1:?${usage}} published=${2:?${usage}} release=${3:?${usage}} settings=${4:?${usage}}
here=$(dirname "$0")
# shellcheck source=common.sh
. "${here}/common.sh"
ctid=${KUBEHENGE_E2E_APPLIANCE_ID:?} address=${KUBEHENGE_E2E_APPLIANCE_ADDRESS:?}
: "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}"
controller_image=${KUBEHENGE_E2E_CONTROLLER_IMAGE:?}
install=${here}/../../appliance/install.sh
ip=${address%/*}
ui=https://${ip}
# The appliance's certificate is signed by the cluster's CA for its address.
ca=/etc/pve/pve-root-ca.pem
admin=kubehenge-e2e-admin@pve
password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
passphrase=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
work=$(mktemp -d)

finish() {
    status=$?
    if [ "${status}" -ne 0 ] && pct status "${ctid}" > /dev/null 2>&1; then
        echo "-- the appliance's log" >&2
        pct exec "${ctid}" -- journalctl -u kubehenge.service -n 100 --no-pager >&2 || true
    fi
    pvesh delete "/access/users/${admin}" > /dev/null 2>&1 || true
    rm -rf "${work}"
}
trap finish EXIT

# management_vms: a line for each VM the management cluster made, which is each QEMU guest the node
# did not hold before the test: its ID, SMBIOS UUID and network cards' MAC addresses. Proxmox VE
# keeps the UUID and the MACs of a VM for its life, so a replaced VM or a re-created card shows.
management_vms() {
    list '.[] | select(.type == "qemu") | "\(.node) \(.vmid)"' /cluster/resources --type vm > "${work}/guests"
    : > "${work}/vms"
    while read -r vm_node vmid; do
        if ! grep -q "^guest ${vmid} " "${work}/before"; then
            printf 'vm %s ' "${vmid}" >> "${work}/vms"
            list '"uuid " + (try (.smbios1 | capture("uuid=(?<u>[^,]+)").u) catch "none") + " macs " +
                ([to_entries[] | select(.key | test("^net[0-9]+$")) | .value | scan("(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}")] | sort | join(","))' \
                "/nodes/${vm_node}/qemu/${vmid}/config" >> "${work}/vms"
        fi
    done < "${work}/guests"
    # Through a file, so that a query that fails stops the test, as list does.
    sort "${work}/vms"
}

# nodes_ready: whether every node of the management cluster is Ready.
# shellcheck disable=SC2310 # a failed read is the poll's answer
nodes_ready() {
    management_get /api/v1/nodes > "${work}/nodes.json" &&
        jq -e '.items | length > 0 and all(.[]; any(.status.conditions[]?; .type == "Ready" and .status == "True"))' "${work}/nodes.json" > /dev/null
}

# providers_ready: whether every Cluster API provider of the management cluster runs the version its
# spec names and is Ready, as lib.sh's providers_ready checks it through kubectl.
# shellcheck disable=SC2310 # a failed read is the poll's answer
providers_ready() {
    : > "${work}/providers.json"
    for kind in coreproviders bootstrapproviders controlplaneproviders infrastructureproviders ipamproviders addonproviders; do
        management_get "/apis/operator.cluster.x-k8s.io/v1alpha2/${kind}" >> "${work}/providers.json" || return 1
    done
    jq -es '[.[].items[]?] | length > 0 and all(.[]; .status.installedVersion == .spec.version and
        any(.status.conditions[]?; .type == "Ready" and .status == "True"))' "${work}/providers.json" > /dev/null
}

if pct status "${ctid}" > /dev/null 2>&1; then
    fail "container ${ctid} exists already"
fi
pvesh create /access/users --userid "${admin}" --password "${password}" > /dev/null
pveum acl modify / --users "${admin}" --roles Administrator
inventory > "${work}/before"

step "install the published release as a user does"
old=$(version_of "${published}")
new=$(version_of "${release}")
install_appliance "${published_install}" "${published}"
runs "${old}"
sign_in "${work}/admin.jar" "${admin}"
admin_csrf=${csrf}

step "set the management cluster up from the published release's interface"
setup=$(jq -c --arg p "${passphrase}" '{passphrase: $p, settings: .}' "${settings}")
expect 202 "starting the setup" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup" "${setup}"
until_true 3600 "the setup to end" job_ended
job_was apply succeeded true
setup_status
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the published release does not record itself as having applied the management cluster" \
    --arg old "${old}" '(.upgrade.behind | not) and .upgrade.applied == $old'

step "the management cluster's VMs, as the published release made them"
management_vms > "${work}/vms-before"
[ -s "${work}/vms-before" ] || fail "the setup made no VM"
if grep -q 'uuid none\|macs $' "${work}/vms-before"; then
    cat "${work}/vms-before" >&2
    fail "a management cluster VM has no SMBIOS UUID or no network card"
fi
cat "${work}/vms-before"

step "upgrade the appliance to ${new} with install.sh --upgrade, under the running management cluster"
upgrade "${release}"
if [ "${upgraded}" -ne 0 ]; then
    cat "${work}/upgrade.log" >&2
    fail "install.sh --upgrade failed"
fi
runs "${new}"
sign_in "${work}/admin.jar" "${admin}"
admin_csrf=${csrf}
until_true 120 "the interface to find the management cluster again" ready_again
upgrade_refused "${published}" 'newer than'
setup_status
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the interface does not say the management cluster runs what ${old} applied" \
    --arg old "${old}" --arg new "${new}" '.upgrade.behind and .upgrade.applied == $old and .upgrade.carried == $new'

step "upgrade the management cluster to what ${new} carries"
# The tree under test's controller image is in no release, so the apply names it, as a release
# would carry its own.
body=$(jq -c --arg p "${passphrase}" --arg image "${controller_image}" '{passphrase: $p, settings: (.settings + {controllerImage: $image})}' "${work}/body")
expect 202 "upgrading the management cluster" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup" "${body}"
until_true 3600 "the management cluster's upgrade to end" job_ended
job_was apply succeeded true
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the management cluster is not recorded as applied by ${new}" \
    --arg new "${new}" '(.upgrade.behind | not) and .upgrade.applied == $new'

step "no VM replaced, no network card re-created, and the management cluster healthy"
management_vms > "${work}/vms-after"
if ! diff "${work}/vms-before" "${work}/vms-after" >&2; then
    fail "the management cluster's VMs changed (< before the upgrade, > after it)"
fi
management_credentials
until_true 600 "every node of the management cluster Ready" nodes_ready
until_true 900 "every provider of the management cluster Ready at its version" providers_ready

step "remove the management cluster from the interface"
body=$(jq -n --arg p "${passphrase}" '{passphrase: $p, confirm: "remove"}')
expect 202 "the removal" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup/remove" "${body}"
until_true 3600 "the removal to end" job_ended
job_was remove succeeded false

step "remove the appliance"
pct stop "${ctid}"
pct destroy "${ctid}" --purge

step "Proxmox VE as it was"
inventory > "${work}/after"
if ! diff "${work}/before" "${work}/after" >&2; then
    fail "Proxmox VE is not left as it was (< before the test, > after it)"
fi

printf '\nPASS\n'
