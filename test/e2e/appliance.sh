#!/bin/sh
# End-to-end test of the appliance on a Proxmox VE node: install it from a release archive, sign in
# with Proxmox VE users, set the management cluster up from the interface, reach the shared Headlamp
# behind the interface's sign-in, upgrade the appliance under the running management cluster, remove
# the management cluster from the interface and then the appliance, and check that Proxmox VE is
# left as it was.
#
#   test/e2e/appliance.sh <release> <newer release> <settings>
#
# Runs as root on the node, as appliance/install.sh does, and needs jq. <release> and <newer release>
# are archives `make dist` built with two versions; <settings> is a JSON file of the management
# cluster's settings as the interface's setup sends them (pkg/appliance/settings.go). The appliance
# is container KUBEHENGE_E2E_APPLIANCE_ID at KUBEHENGE_E2E_APPLIANCE_ADDRESS (with its prefix length)
# on KUBEHENGE_E2E_BRIDGE (default vmbr0), with the gateway KUBEHENGE_E2E_GATEWAY and the name
# servers KUBEHENGE_E2E_DNS; its disk goes on KUBEHENGE_E2E_STORAGE when that is set.
#
# A failed run leaves the appliance and whatever it set up, to look at; remove them as
# docs/uninstalling.md says before running it again.
set -eu

usage="usage: $0 <release> <newer release> <settings>"
release=${1:?${usage}} newer=${2:?${usage}} settings=${3:?${usage}}
here=$(dirname "$0")
# shellcheck source=common.sh
. "${here}/common.sh"
ctid=${KUBEHENGE_E2E_APPLIANCE_ID:?} address=${KUBEHENGE_E2E_APPLIANCE_ADDRESS:?}
: "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}"
install=${here}/../../appliance/install.sh
ip=${address%/*}
ui=https://${ip} headlamp=https://${ip}:8443
# The appliance's certificate is signed by the cluster's CA for its address.
ca=/etc/pve/pve-root-ca.pem
admin=kubehenge-e2e-admin@pve user=kubehenge-e2e-user@pve
password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
passphrase=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
work=$(mktemp -d)

finish() {
    status=$?
    if [ "${status}" -ne 0 ] && pct status "${ctid}" > /dev/null 2>&1; then
        echo "-- the appliance's log" >&2
        pct exec "${ctid}" -- journalctl -u kubehenge.service -n 100 --no-pager >&2 || true
    fi
    for u in "${admin}" "${user}"; do
        pvesh delete "/access/users/${u}" > /dev/null 2>&1 || true
    done
    rm -rf "${work}"
}
trap finish EXIT

# request <cookie jar> <curl arguments...>: a request to the appliance; its status goes to
# $work/status and its body to $work/body.
request() {
    jar=$1
    shift
    curl -sS --cacert "${ca}" -b "${jar}" -c "${jar}" -o "${work}/body" -w '%{http_code}' "$@" > "${work}/status"
}

# post <cookie jar> <CSRF token> <URL> <JSON>
post() {
    request "$1" -H 'Content-Type: application/json' -H "X-Kubehenge-CSRF: $2" -d "$4" "$3"
}

# expect <status> <what> <command...>: runs a request and checks the status it got.
expect() {
    want=$1 what=$2
    shift 2
    "$@"
    got=$(cat "${work}/status")
    [ "${got}" = "${want}" ] || fail_with_body "${what}: ${got} instead of ${want}"
}

# fail_with_body <message>: fails with the start of the last answer's body.
fail_with_body() {
    body=$(head -c 500 "${work}/body")
    fail "$1: ${body}"
}

# body_is <jq filter> <message>: checks the last answer's body.
body_is() {
    jq -e "$1" "${work}/body" > /dev/null || fail_with_body "$2"
}

# answers: whether the interface is up again, on its certificate.
answers() {
    curl -sS -o /dev/null --cacert "${ca}" "${ui}/api/realms" 2> /dev/null
}

# sign_in <cookie jar> <user>: signs in with the test's password, the session in the jar and its
# CSRF token in $csrf.
sign_in() {
    body=$(jq -n --arg u "$2" --arg p "${password}" '{username: $u, password: $p}')
    expect 200 "signing in as $2" request "$1" -H 'Content-Type: application/json' -d "${body}" "${ui}/api/session"
    csrf=$(jq -r .csrf "${work}/body")
}

# setup_status: the setup's status as the administrator sees it, in $work/body.
setup_status() {
    expect 200 "the setup's status" request "${work}/admin.jar" "${ui}/api/setup"
}

# job_ended: whether the appliance's last job has ended.
job_ended() {
    setup_status
    jq -e '.job.state != "running"' "${work}/body" > /dev/null
}

# job_was <kind> <state> <ready>: checks how the last job ended and whether the management cluster
# is ready after it, with the job's log when not as expected.
job_was() {
    if ! jq -e --arg k "$1" --arg s "$2" --argjson r "$3" '.job.kind == $k and .job.state == $s and .ready == $r' "${work}/body" > /dev/null; then
        jq -r '"kind \(.job.kind), state \(.job.state), ready \(.ready): \(.job.error // "")", (.job.log // [] | .[-40:][])' "${work}/body" >&2
        fail "the $1 job did not end $2 with the management cluster ready: $3"
    fi
}

# ready_again: whether the interface, after a restart, found the management cluster again.
ready_again() {
    setup_status
    jq -e .ready "${work}/body" > /dev/null
}

# headlamp_serves: whether Headlamp's own page comes through the interface's sign-in.
headlamp_serves() {
    request "${work}/admin.jar" "${headlamp}/"
    got=$(cat "${work}/status")
    [ "${got}" = 200 ] && grep -q Headlamp "${work}/body"
}

# upgrade <release>: install.sh --upgrade of the appliance, its exit status in $upgraded and its
# output in $work/upgrade.log.
upgrade() {
    sum=$(sha256sum "$1" | cut -d ' ' -f 1)
    upgraded=0
    sh "${install}" --upgrade --ctid "${ctid}" --release "$1" --sha256 "${sum}" < /dev/null > "${work}/upgrade.log" 2>&1 || upgraded=$?
}

# upgrade_refused <release> <reason>: checks that install.sh --upgrade stops, giving the reason.
upgrade_refused() {
    upgrade "$1"
    if [ "${upgraded}" -eq 0 ] || ! grep -q "$2" "${work}/upgrade.log"; then
        cat "${work}/upgrade.log" >&2
        fail "install.sh --upgrade to $1 did not stop with: $2"
    fi
}

# list <jq filter> <pvesh get arguments...>: an answer of Proxmox VE's API as lines. Through a file
# rather than a pipe, so that a query that fails stops the test instead of leaving lines out.
list() {
    filter=$1
    shift
    pvesh get "$@" --output-format json < /dev/null > "${work}/answer"
    jq -r "${filter}" "${work}/answer"
}

# inventory: what Proxmox VE holds that the appliance or the management cluster could leave behind,
# a line each. Container templates stay: install.sh keeps the one it downloaded for the next install.
inventory() {
    node=$(hostname)
    list '.[] | select(.active == 1) | .storage' "/nodes/${node}/storage" --enabled 1 > "${work}/storages"
    {
        list '.[] | "guest \(.vmid) \(.name // "")"' /cluster/resources --type vm
        list '.[] | "pool \(.poolid)"' /pools
        list '.[] | "user \(.userid) \(.tokens // [] | map(.tokenid) | sort | join(","))"' /access/users --full 1
        list '.[] | "group \(.groupid)"' /access/groups
        list '.[] | "role \(.roleid)"' /access/roles
        list '.[] | "acl \(.path) \(.type) \(.ugid) \(.roleid)"' /access/acl
        list '.[] | "ha resource \(.sid)"' /cluster/ha/resources
        list '.[] | "ha rule \(.rule)"' /cluster/ha/rules
        while read -r storage; do
            list '.[] | select(.content != "vztmpl") | "volume \(.volid)"' "/nodes/${node}/storage/${storage}/content"
        done < "${work}/storages"
    } > "${work}/inventory"
    sort "${work}/inventory"
}

if pct status "${ctid}" > /dev/null 2>&1; then
    fail "container ${ctid} exists already"
fi
pvesh create /access/users --userid "${admin}" --password "${password}" > /dev/null
pveum acl modify / --users "${admin}" --roles Administrator
pvesh create /access/users --userid "${user}" --password "${password}" > /dev/null
inventory > "${work}/before"

step "install the appliance"
sum=$(sha256sum "${release}" | cut -d ' ' -f 1)
set -- --ctid "${ctid}" --ip "${address}" --gateway "${KUBEHENGE_E2E_GATEWAY}" --dns "${KUBEHENGE_E2E_DNS}" \
    --bridge "${KUBEHENGE_E2E_BRIDGE:-vmbr0}" --hostname kubehenge-e2e --release "${release}" --sha256 "${sum}" --no-pve-entry
[ -z "${KUBEHENGE_E2E_STORAGE:-}" ] || set -- "$@" --storage "${KUBEHENGE_E2E_STORAGE}"
if ! sh "${install}" "$@" < /dev/null > "${work}/install.log" 2>&1; then
    cat "${work}/install.log" >&2
    fail "install.sh failed"
fi
grep -qF "Kubehenge is running in container ${ctid}: ${ui}/" "${work}/install.log" ||
    fail "install.sh did not name the appliance's address"
pct config "${ctid}" > "${work}/config"
nameservers=$(printf '%s' "${KUBEHENGE_E2E_DNS}" | tr , ' ')
for want in 'tags: kubehenge' 'unprivileged: 1' "nameserver: ${nameservers}"; do
    grep -qxF "${want}" "${work}/config" || fail "the container's configuration has no '${want}'"
done

step "the interface and Headlamp's port, on a certificate the cluster's CA signed for the address"
expect 401 "the session before signing in" request "${work}/none.jar" "${ui}/api/session"
expect 303 "Headlamp before signing in" request "${work}/none.jar" "${headlamp}/"

step "sign-in with Proxmox VE users, and what they may do"
body=$(jq -n --arg u "${admin}" '{username: $u, password: "not the password"}')
expect 401 "a wrong password" request "${work}/none.jar" -H 'Content-Type: application/json' -d "${body}" "${ui}/api/session"
sign_in "${work}/user.jar" "${user}"
user_csrf=${csrf}
sign_in "${work}/admin.jar" "${admin}"
admin_csrf=${csrf}
expect 200 "the setup's status for a user without privileges" request "${work}/user.jar" "${ui}/api/setup"
body_is '.appliance and (.allowed | not) and .job == null' "a user without privileges sees the setup"
setup=$(jq -c --arg p "${passphrase}" '{passphrase: $p, settings: .}' "${settings}")
expect 403 "a setup by a user without privileges" post "${work}/user.jar" "${user_csrf}" "${ui}/api/setup" "${setup}"
expect 403 "a setup without the session's CSRF token" post "${work}/admin.jar" '' "${ui}/api/setup" "${setup}"
setup_status
body_is '.allowed and (.ready | not)' "the administrator may not set up"

step "set the management cluster up from the interface"
expect 202 "starting the setup" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup" "${setup}"
expect 409 "a second setup beside it" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup" "${setup}"
upgrade_refused "${newer}" 'is being applied or removed'
until_true 3600 "the setup to end" job_ended
job_was apply succeeded true
expect 200 "the ClusterClasses" request "${work}/admin.jar" "${ui}/api/classes"
body_is '[.[].name] | contains(["kubehenge-kubeadm", "kubehenge-rke2", "kubehenge-k3s"])' "the interface does not offer the ClusterClasses"

step "the shared Headlamp behind the interface's sign-in"
# Its service starts on the first request.
until_true 120 "Headlamp's page through the gate" headlamp_serves
expect 200 "Headlamp's configuration" request "${work}/admin.jar" "${headlamp}/config"
body_is 'has("clusters")' "Headlamp's configuration has no clusters"
expect 403 "Headlamp's plugin upload" request "${work}/admin.jar" "${headlamp}/user-plugins/x"

step "upgrade the appliance with the management cluster up"
upgrade "${newer}"
if [ "${upgraded}" -ne 0 ]; then
    cat "${work}/upgrade.log" >&2
    fail "install.sh --upgrade failed"
fi
version=$(tar -xzOf "${newer}" ./share/kubehenge/VERSION)
installed=$(pct exec "${ctid}" -- cat /usr/local/share/kubehenge/VERSION)
[ "${installed}" = "${version}" ] || fail "the appliance runs ${installed} instead of ${version}"
until_true 120 "the interface to answer again" answers
sign_in "${work}/admin.jar" "${admin}"
admin_csrf=${csrf}
until_true 120 "the interface to find the management cluster again" ready_again
upgrade_refused "${release}" 'newer than'

step "remove the management cluster from the interface"
body=$(jq -n --arg p "${passphrase}" '{passphrase: $p, confirm: "delete"}')
expect 422 "a removal without the word remove" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup/remove" "${body}"
body=$(jq -n --arg p "not-${passphrase}" '{passphrase: $p, confirm: "remove"}')
expect 202 "a removal with a wrong passphrase" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup/remove" "${body}"
until_true 600 "the removal with a wrong passphrase to end" job_ended
job_was remove failed true
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
