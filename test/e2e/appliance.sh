#!/bin/sh
# End-to-end test of the appliance on a Proxmox VE node: install it from a release archive, sign in
# with Proxmox VE users, set the management cluster up from the interface, place by an HA rule
# offered to a cluster creator, reach the shared Headlamp behind the interface's sign-in, with the
# plugins it ships and its Helm routes' limits, and log out of a cluster in it, snapshot the
# management cluster's etcd into an S3 store on the node and restore it from the appliance, look
# after both from the appliance's shell, switch automatic upgrades of the management cluster on and
# off, upgrade the appliance under the running management cluster through its own updater and
# through install.sh, each rolling back a release whose interface does not start first, and then
# the management cluster to what it carries, remove the management cluster from the interface and
# then the appliance, and check that Proxmox VE is left as it was.
#
#   test/e2e/appliance.sh <release> <newer release> <newest release> <settings>
#
# Runs as root on the node, as appliance/install.sh does, and needs jq. The releases are archives
# `make dist` built with three versions, in order, and with RELEASE_KEY the public half of the
# Ed25519 key in KUBEHENGE_E2E_RELEASE_KEY, as `kubehenge release keygen` wrote it, which signs the
# index the test serves the appliance's updater; <settings> is a JSON file of the management
# cluster's settings as the interface's setup sends them (pkg/appliance/settings.go), with
# offerHARules and headlampActions on and the default workload pool. The appliance
# is container KUBEHENGE_E2E_APPLIANCE_ID at KUBEHENGE_E2E_APPLIANCE_ADDRESS (with its prefix length)
# on KUBEHENGE_E2E_BRIDGE (default vmbr0), with the gateway KUBEHENGE_E2E_GATEWAY and the name
# servers KUBEHENGE_E2E_DNS; its disk goes on KUBEHENGE_E2E_STORAGE when that is set. The S3 store
# serves on the node at KUBEHENGE_E2E_S3_ADDRESS, an address of the node's the appliance reaches.
#
# A failed run leaves the appliance and whatever it set up, to look at; remove them as
# docs/uninstalling.md says before running it again.
set -eu

usage="usage: $0 <release> <newer release> <newest release> <settings>"
release=${1:?${usage}} newer=${2:?${usage}} newest=${3:?${usage}} settings=${4:?${usage}}
release_key=${KUBEHENGE_E2E_RELEASE_KEY:?}
here=$(dirname "$0")
# shellcheck source=common.sh
. "${here}/common.sh"
ctid=${KUBEHENGE_E2E_APPLIANCE_ID:?} address=${KUBEHENGE_E2E_APPLIANCE_ADDRESS:?}
: "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}"
s3_endpoint=https://${KUBEHENGE_E2E_S3_ADDRESS:?}:8333 s3_pid=
install=${here}/../../appliance/install.sh
ip=${address%/*}
ui=https://${ip} headlamp=https://${ip}:8443
# The appliance's certificate is signed by the cluster's CA for its address.
ca=/etc/pve/pve-root-ca.pem
admin=kubehenge-e2e-admin@pve user=kubehenge-e2e-user@pve upgrader=kubehenge-upgrader@pve
# The HA rule offered to cluster creators, and the guest it needs: Proxmox VE takes a rule only for
# HA resources, and a resource only for a guest that exists.
ha_rule=kubehenge-e2e-placement ha_vmid=
password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
passphrase=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
work=$(mktemp -d)

finish() {
    status=$?
    if [ "${status}" -ne 0 ] && pct status "${ctid}" > /dev/null 2>&1; then
        echo "-- the appliance's log" >&2
        pct exec "${ctid}" -- journalctl -u kubehenge.service -n 100 --no-pager >&2 || true
    fi
    [ "${status}" -eq 0 ] || seaweedfs_errors "${work}"
    if [ -n "${ha_vmid}" ]; then
        pvesh delete "/cluster/ha/rules/${ha_rule}" > /dev/null 2>&1 || true
        pvesh delete "/cluster/ha/resources/vm:${ha_vmid}" > /dev/null 2>&1 || true
        qm destroy "${ha_vmid}" --purge > /dev/null 2>&1 || true
    fi
    for u in "${admin}" "${user}"; do
        pvesh delete "/access/users/${u}" > /dev/null 2>&1 || true
    done
    [ -z "${s3_pid}" ] || kill "${s3_pid}" 2> /dev/null || true
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

# post <cookie jar> <CSRF token> <URL> <JSON>, and put and delete the same
post() {
    request "$1" -H 'Content-Type: application/json' -H "X-Kubehenge-CSRF: $2" -d "$4" "$3"
}
put() {
    request "$1" -X PUT -H 'Content-Type: application/json' -H "X-Kubehenge-CSRF: $2" -d "$4" "$3"
}
delete() {
    request "$1" -X DELETE -H "X-Kubehenge-CSRF: $2" "$3"
}

# headlamp_post <cookie jar> <URL> <JSON> [curl arguments...]: a request that changes something, as
# Headlamp's own pages post it, from their origin.
headlamp_post() {
    jar=$1 url=$2 json=$3
    shift 3
    request "${jar}" -H "Origin: ${headlamp}" -H 'Content-Type: application/json' -d "${json}" "$@" "${url}"
}

# upgrader_is <present|absent> <message>: checks whether Proxmox VE has the user automatic upgrades
# of the management cluster act as.
upgrader_is() {
    if pvesh get "/access/users/${upgrader}" --output-format json < /dev/null > /dev/null 2>&1; then
        [ "$1" = present ] || fail "$2"
    else
        [ "$1" = absent ] || fail "$2"
    fi
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

# body_has <message> <jq arguments...>: body_is with a filter that takes arguments.
body_has() {
    message=$1
    shift
    jq -e "$@" "${work}/body" > /dev/null || fail_with_body "${message}"
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
        jq -r '"kind \(.job.kind), state \(.job.state), ready \(.ready): \(.job.error // "")", (.job.log // [] | .[])' "${work}/body" >&2
        fail "the $1 job did not end $2 with the management cluster ready: $3"
    fi
}

# ready_again: whether the interface, after a restart, found the management cluster again.
ready_again() {
    setup_status
    jq -e .ready "${work}/body" > /dev/null
}

# s3 <curl arguments...>: a request to the S3 store, signed with the test's key.
s3() {
    curl -sS --fail --cacert "${work}/s3/ca.crt" --aws-sigv4 aws:amz:us-east-1:s3 --user "${s3_key}:${s3_secret}" "$@"
}

# snapshot_taken <name>: whether the backups of the management cluster's etcd took a snapshot other
# than the one named; one that failed fails the test with why.
snapshot_taken() {
    expect 200 "the etcd backups" request "${work}/admin.jar" "${ui}/api/appliance/etcd-backups"
    if jq -e '(.error // "") != ""' "${work}/body" > /dev/null; then
        fail_with_body "the snapshot failed"
    fi
    jq -e --arg not "$1" '(.taking | not) and .last.name != null and .last.name != $not' "${work}/body" > /dev/null
}

# dns01_set <true|false>: whether the DNS-01 credential for every cluster, a Secret in the management
# cluster, is stored, as the interface reads it.
dns01_set() {
    request "${work}/admin.jar" "${ui}/api/credentials/dns01"
    got=$(cat "${work}/status")
    [ "${got}" = 200 ] && jq -e --argjson set "$1" '(.set // false) == $set' "${work}/body" > /dev/null
}

# headlamp_serves: whether Headlamp's own page comes through the interface's sign-in.
headlamp_serves() {
    request "${work}/admin.jar" "${headlamp}/"
    got=$(cat "${work}/status")
    [ "${got}" = 200 ] && grep -q Headlamp "${work}/body"
}

# headlamp_offers_actions: whether Headlamp's plugin list holds the app catalog it ships, which the
# gate leaves out until it has read that the actions are on.
headlamp_offers_actions() {
    request "${work}/admin.jar" "${headlamp}/plugins"
    got=$(cat "${work}/status")
    [ "${got}" = 200 ] && jq -e 'any(.[]; .path == "static-plugins/app-catalog")' "${work}/body" > /dev/null
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

# upgrade_rolled_back <release> <version installed>: checks that install.sh --upgrade put the
# release installed back, saying why.
upgrade_rolled_back() {
    upgrade "$1"
    to=$(version_of "$1")
    if [ "${upgraded}" -eq 0 ] || ! grep -q "release ${to} did not start: the interface did not answer within .*; rolled back to $2\$" "${work}/upgrade.log"; then
        cat "${work}/upgrade.log" >&2
        fail "install.sh --upgrade to ${to} did not roll back to $2"
    fi
    runs "$2"
}

# version_of <release>: the version of a release archive.
version_of() { tar -xzOf "$1" ./share/kubehenge/VERSION; }

# runs <version>: checks that the appliance runs that release, its interface answering.
runs() {
    installed=$(pct exec "${ctid}" -- cat /usr/local/share/kubehenge/VERSION)
    [ "${installed}" = "$1" ] || fail "the appliance runs ${installed} instead of $1"
    until_true 120 "the interface to answer" answers
}

# cli <user> <arguments...>: the kubehenge appliance command in the appliance, signed in as the user
# with the test's password, the state passphrase at hand; its output in $work/cli and whether it
# succeeded in $cli_ok.
cli() {
    who=$1
    shift
    cli_ok=true
    pct exec "${ctid}" -- /usr/local/bin/kubehenge appliance --user "${who}" --password-file /root/e2e-password "$@" \
        > "${work}/cli" 2>&1 < /dev/null || cli_ok=false
}

# cli_says <true|false> <text> <what>: checks how the last cli ended and that it said the text.
cli_says() {
    if [ "${cli_ok}" != "$1" ] || ! grep -qF -- "$2" "${work}/cli"; then
        head -c 2000 "${work}/cli" >&2
        fail "$3"
    fi
}

# upgrade_status: the updater's last result as the interface shows it, in $work/body, signing in
# again once a restart ended the session; fails while the interface does not answer.
# shellcheck disable=SC2310 # a request the restarting interface does not answer is the poll's answer
upgrade_status() {
    request "${work}/admin.jar" "${ui}/api/appliance/upgrades" 2> /dev/null || return 1
    got=$(cat "${work}/status")
    if [ "${got}" = 401 ]; then
        body=$(jq -n --arg u "${admin}" --arg p "${password}" '{username: $u, password: $p}')
        request "${work}/admin.jar" -H 'Content-Type: application/json' -d "${body}" "${ui}/api/session" || return 1
        admin_csrf=$(jq -r '.csrf // ""' "${work}/body")
        request "${work}/admin.jar" "${ui}/api/appliance/upgrades" || return 1
        got=$(cat "${work}/status")
    fi
    [ "${got}" = 200 ]
}

# updater_done <version>: whether the updater's upgrade to the version has ended.
# shellcheck disable=SC2310 # as upgrade_status
updater_done() {
    upgrade_status && jq -e --arg to "$1" '.status.to == $to and .status.state != "updating"' "${work}/body" > /dev/null
}

# list <jq filter> <pvesh get arguments...>: an answer of Proxmox VE's API as lines. Through a file
# rather than a pipe, so that a query that fails stops the test instead of leaving lines out.
list() {
    filter=$1
    shift
    pvesh get "$@" --output-format json < /dev/null > "${work}/answer"
    jq -r "${filter}" "${work}/answer"
}

# remove_ha_rule: takes the test's HA rule, its resource and guest away again, in the order Proxmox
# VE allows.
remove_ha_rule() {
    [ -n "${ha_vmid}" ] || return 0
    pvesh delete "/cluster/ha/rules/${ha_rule}" > /dev/null
    pvesh delete "/cluster/ha/resources/vm:${ha_vmid}" > /dev/null
    qm destroy "${ha_vmid}" --purge > /dev/null
    ha_vmid=
}

# rules_offered <node>: whether the user without privileges on / is offered the test's HA rule, by
# its name and node alone.
rules_offered() {
    request "${work}/user.jar" "${ui}/api/proxmox/ha-rules"
    got=$(cat "${work}/status")
    [ "${got}" = 200 ] &&
        jq -e --arg r "${ha_rule}" --arg n "$1" \
            '.readable and .offered and .rules == [{rule: $r, nodes: [$n], allowed: [$n]}]' "${work}/body" > /dev/null
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

step "an S3 store on the node, offered as a backup target"
mkdir -p "${work}/s3"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=kubehenge-e2e-s3-ca \
    -keyout "${work}/s3/ca.key" -out "${work}/s3/ca.crt" 2> /dev/null
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=kubehenge-e2e-s3 \
    -keyout "${work}/s3/tls.key" -out "${work}/s3/tls.csr" 2> /dev/null
printf 'subjectAltName = IP:%s\nextendedKeyUsage = serverAuth\n' "${KUBEHENGE_E2E_S3_ADDRESS}" > "${work}/s3/san.cnf"
openssl x509 -req -in "${work}/s3/tls.csr" -CA "${work}/s3/ca.crt" -CAkey "${work}/s3/ca.key" -CAcreateserial \
    -CAserial "${work}/s3/ca.srl" -days 2 -extfile "${work}/s3/san.cnf" -out "${work}/s3/tls.crt" 2> /dev/null
seaweedfs "${work}"
s3_key=e2e$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')
s3_secret=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
# Anyone may read the releases bucket, as anyone may download a published release.
jq -n --arg k "${s3_key}" --arg s "${s3_secret}" '{identities: [
    {name: "e2e", credentials: [{accessKey: $k, secretKey: $s}], actions: ["Admin", "Read", "Write", "List", "Tagging"]},
    {name: "anonymous", actions: ["Read:releases"]}]}' > "${work}/s3/s3.json"
seaweedfs_server "${work}" 8333 "${work}/s3/s3.json" "${work}/s3/tls.crt" "${work}/s3/tls.key"
s3_pid=$!
until_true 120 "the S3 store takes a bucket" s3 -X PUT -o /dev/null "${s3_endpoint}/kubehenge-e2e"
# shellcheck disable=SC2016 # jq's variables, not the shell's.
jq --arg e "${s3_endpoint}" --rawfile ca "${work}/s3/ca.crt" \
    '.backupTargets = [{name: "e2e", endpoint: $e, bucket: "kubehenge-e2e", prefix: "ci", caCertificate: $ca}]' \
    "${settings}" > "${work}/settings.json"

step "releases for the appliance's updater, in an index signed with the test's release key"
first_version=$(version_of "${release}")
newer_version=$(version_of "${newer}")
newest_version=$(version_of "${newest}")
# A release whose interface does not start, newer than all three: the newest with its binary
# behind a wrapper that fails to serve and does all else, the health check included.
broken_version=$(printf '%s' "${newest_version}" | awk -F. '{ printf "%d.%d.%d", $1, $2, $3 + 1 }')
mkdir "${work}/broken"
tar -xzf "${newest}" -C "${work}/broken"
mkdir "${work}/broken/share/kubehenge/broken"
mv "${work}/broken/bin/kubehenge" "${work}/broken/share/kubehenge/broken/kubehenge"
cat > "${work}/broken/bin/kubehenge" << 'EOF'
#!/bin/sh
if [ "${1:-}" = serve ]; then
    echo "kubehenge: this release is broken on purpose" >&2
    exit 1
fi
exec /usr/local/share/kubehenge/broken/kubehenge "$@"
EOF
chmod 0755 "${work}/broken/bin/kubehenge"
echo "${broken_version}" > "${work}/broken/share/kubehenge/VERSION"
broken=${work}/kubehenge_${broken_version}_linux_amd64.tar.gz
tar -czf "${broken}" --owner=0 --group=0 -C "${work}/broken" .
rm -rf "${work}/broken"
# The updater is offered the newer release and the broken one; install.sh is given the others.
releases=${s3_endpoint}/releases
until_true 60 "the S3 store takes the releases bucket" s3 -X PUT -o /dev/null "${releases}"
for archive in "${newer}" "${broken}"; do
    s3 -T "${archive}" -o /dev/null "${releases}/${archive##*/}"
done
newer_sum=$(sha256sum "${newer}" | cut -d ' ' -f 1)
broken_sum=$(sha256sum "${broken}" | cut -d ' ' -f 1)
# shellcheck disable=SC2016 # jq's variables, not the shell's.
jq -n --arg base "${releases}" --arg nv "${newer_version}" --arg na "${newer##*/}" --arg ns "${newer_sum}" \
    --arg bv "${broken_version}" --arg ba "${broken##*/}" --arg bs "${broken_sum}" \
    '{releases: [{version: $nv, url: "\($base)/\($na)", sha256: $ns}, {version: $bv, url: "\($base)/\($ba)", sha256: $bs}]}' \
    > "${work}/index.json"
# Signed by the first release's own binary, which checks the key against the one it trusts.
tar -xzOf "${release}" ./bin/kubehenge > "${work}/kubehenge"
chmod 0755 "${work}/kubehenge"
"${work}/kubehenge" release sign "${release_key}" "${work}/index.json"
s3 -T "${work}/index.json" -o /dev/null "${releases}/index.json"
s3 -T "${work}/index.json.sig" -o /dev/null "${releases}/index.json.sig"

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
# For the kubehenge appliance commands run in it.
for secret in "password:${password}" "passphrase:${passphrase}" "wrong-passphrase:not-${passphrase}"; do
    printf '%s\n' "${secret#*:}" > "${work}/secret"
    pct push "${ctid}" "${work}/secret" "/root/e2e-${secret%%:*}" --perms 0600
done
rm "${work}/secret"

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
setup=$(jq -c --arg p "${passphrase}" '{passphrase: $p, settings: .}' "${work}/settings.json")
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
# The settings' extraClusterNamespaces, after the interface's own.
expected_namespaces=$(jq -c '["clusters"] + (.extraClusterNamespaces // [])' "${settings}")
expect 200 "the session" request "${work}/admin.jar" "${ui}/api/session"
body_is ".namespace == \"clusters\" and .namespaces == ${expected_namespaces}" "the interface does not offer the namespaces ${expected_namespaces}"

step "an HA rule offered to a cluster creator without Sys.Audit on /, by its name and nodes alone"
expect 200 "the HA rules for a user who may place no cluster" request "${work}/user.jar" "${ui}/api/proxmox/ha-rules"
body_is '(.readable | not) and .rules == []' "a user who may place no cluster is offered the HA rules"
node=$(hostname)
ha_vmid=$(pvesh get /cluster/nextid --output-format json | jq -r .)
qm create "${ha_vmid}" --name kubehenge-e2e-ha-rule --memory 64 > /dev/null
# Ignored, so that Proxmox VE HA neither starts the guest nor arms a node's watchdog for it.
pvesh create /cluster/ha/resources --sid "vm:${ha_vmid}" --state ignored > /dev/null
pvesh create /cluster/ha/rules --type node-affinity --rule "${ha_rule}" --resources "vm:${ha_vmid}" \
    --nodes "${node}" --strict 1 --comment 'not for cluster creators' > /dev/null
# The workload pool the setup made, where VM.Allocate lets the user create a cluster.
pveum acl modify /pool/kubehenge --users "${user}" --roles PVEVMAdmin
until_true 180 "the HA rule offered to the cluster creator" rules_offered "${node}"
expect 200 "the HA rules for the administrator" request "${work}/admin.jar" "${ui}/api/proxmox/ha-rules"
body_is "(.offered | not) and any(.rules[]; .rule == \"${ha_rule}\" and .comment == \"not for cluster creators\" and .strict)" \
    "the administrator, who holds Sys.Audit on /, does not read the HA rule in full from Proxmox VE"
pveum acl delete /pool/kubehenge --users "${user}" --roles PVEVMAdmin
remove_ha_rule

step "the shared Headlamp behind the interface's sign-in"
# Its service starts on the first request.
until_true 120 "Headlamp's page through the gate" headlamp_serves
expect 200 "Headlamp's configuration" request "${work}/admin.jar" "${headlamp}/config"
body_is 'has("clusters")' "Headlamp's configuration has no clusters"
expect 403 "Headlamp's plugin upload" request "${work}/admin.jar" "${headlamp}/user-plugins/x"

step "the shared Headlamp's actions"
# The settings switch them on; the interface reads that from the chart's ConfigMap every half minute.
until_true 120 "the plugins Headlamp ships in its plugin list" headlamp_offers_actions
expect 200 "the app catalog" request "${work}/admin.jar" "${headlamp}/static-plugins/app-catalog/main.js"
expect 200 "the Prometheus charts" request "${work}/admin.jar" "${headlamp}/static-plugins/prometheus/main.js"
expect 403 "a chart from a path on the appliance" headlamp_post "${work}/admin.jar" \
    "${headlamp}/clusters/clusters.e2e/helm/release/install?namespace=default" \
    '{"name":"x","namespace":"default","chart":"/etc","version":"1","description":"x"}'
expect 403 "a Helm repository over HTTP" headlamp_post "${work}/admin.jar" \
    "${headlamp}/clusters/clusters.e2e/helm/repositories" '{"name":"x","url":"http://charts.example"}'
expect 403 "a port forward" headlamp_post "${work}/admin.jar" \
    "${headlamp}/clusters/clusters.e2e/portforward" '{"namespace":"default","pod":"x","targetPort":"80"}'

step "Headlamp's log out of a cluster through the gate, and opening the cluster again"
# Headlamp's frontend names the cluster in unpadded URL-safe base64 and posts JSON. The gate judges
# a cluster by its pool, which the administrator holds on / whether or not the cluster exists.
logout=${headlamp}/encoded-clusters/$(printf clusters.e2e | base64 -w0 | tr '+/' '-_' | tr -d '=')/clear-token
# The interface's own port is another origin of the same site, which the session cookie reaches.
expect 403 "Headlamp's log out from the interface's origin" request "${work}/admin.jar" \
    -H "Origin: ${ui}" -H 'Content-Type: application/json' -d '{"token":null}' "${logout}"
expect 403 "Headlamp's log out without an origin" request "${work}/admin.jar" \
    -H 'Content-Type: application/json' -d '{"token":null}' "${logout}"
expect 200 "Headlamp's log out" headlamp_post "${work}/admin.jar" "${logout}" '{"token":null}' -D "${work}/headers"
grep -qi '^set-cookie: headlamp-auth-clusterse2e\.0=;' "${work}/headers" ||
    fail "Headlamp's log out did not clear the cluster's token cookie"
expect 403 "Headlamp's log out by GET" request "${work}/admin.jar" "${logout}"
expect 404 "Headlamp's log out of a cluster the user does not see" headlamp_post "${work}/user.jar" "${logout}" '{"token":null}'
expect 303 "opening the cluster from the interface" request "${work}/admin.jar" -D "${work}/headers" \
    "${headlamp}/kubehenge/open/clusters.e2e/pods"
grep -qi '^location: /c/clusters\.e2e/pods' "${work}/headers" ||
    fail "opening the cluster did not lead on to its page in Headlamp"
expect 404 "opening a cluster the user does not see" request "${work}/user.jar" "${headlamp}/kubehenge/open/clusters.e2e"

step "the management cluster's etcd, snapshotted into the backup target"
backups=${ui}/api/appliance/etcd-backups
expect 403 "the etcd backups for a user without privileges" request "${work}/user.jar" "${backups}"
# shellcheck disable=SC2016 # jq's variables, not the shell's.
backup_settings='{target: "e2e", everyHours: 6, keep: 2, accessKeyID: $k, secretAccessKey: $s, passphrase: $p}'
body=$(jq -n --arg k "${s3_key}" --arg s "${s3_secret}" --arg p "not-${passphrase}" "${backup_settings}")
expect 403 "setting them up with a wrong passphrase" put "${work}/admin.jar" "${admin_csrf}" "${backups}" "${body}"
body=$(jq -n --arg k "${s3_key}" --arg s "${s3_secret}" --arg p "${passphrase}" "${backup_settings}")
expect 200 "setting them up" put "${work}/admin.jar" "${admin_csrf}" "${backups}" "${body}"
body_is '.settings.target == "e2e" and .keysStored' "the backups are not set up"
# shellcheck disable=SC2310 # its failure is the test's, with a message of its own.
s3 -o /dev/null "${s3_endpoint}/kubehenge-e2e/ci/management/identity.age" ||
    fail "the target holds no key for the snapshots"
# The first is due at once, and the appliance looks every minute.
until_true 300 "the first scheduled snapshot" snapshot_taken ''
first=$(jq -r .last.name "${work}/body")
# shellcheck disable=SC2310 # as above
s3 -o "${work}/snapshot" "${s3_endpoint}/kubehenge-e2e/ci/management/etcd/${first}" ||
    fail "the target does not hold ${first}"
magic=$(head -c 21 "${work}/snapshot")
[ "${magic}" = age-encryption.org/v1 ] || fail "${first} is not encrypted"
expect 202 "a snapshot now" post "${work}/admin.jar" "${admin_csrf}" "${backups}/snapshots" ''
body_is '.taking' "the snapshot asked for is not being taken"
until_true 300 "the snapshot asked for" snapshot_taken "${first}"
second=$(jq -r .last.name "${work}/body")
expect 200 "the snapshots in the target" request "${work}/admin.jar" "${backups}/snapshots"
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the target does not list both snapshots" --arg a "${first}" --arg b "${second}" \
    '[.snapshots[].name] == [$b, $a] and (.error // "") == ""'

step "the management cluster's etcd restored from the appliance"
# A Secret in the management cluster made after the snapshot, which the restore takes back.
expect 200 "storing the DNS-01 credential" put "${work}/admin.jar" "${admin_csrf}" "${ui}/api/credentials/dns01" \
    '{"values":{"token":"e2e"}}'
until_true 30 "the DNS-01 credential stored" dns01_set true
restore=${backups}/snapshots/${second}/restore
body=$(jq -n --arg p "not-${passphrase}" '{passphrase: $p}')
expect 403 "restoring with a wrong passphrase" post "${work}/admin.jar" "${admin_csrf}" "${restore}" "${body}"
refusal=$(jq -r .error "${work}/body")
# From the appliance's shell, refused in the interface's words, and then restored.
cli "${admin}" etcd-backups restore "${second}" --passphrase-file /root/e2e-wrong-passphrase
cli_says false "${refusal}" "the shell's restore with a wrong passphrase was not refused as the interface's"
cli "${admin}" etcd-backups restore "${second}" --passphrase-file /root/e2e-passphrase
cli_says true "Restored from ${second}" "the shell's restore did not end restored"
setup_status
job_was etcd-restore succeeded true
until_true 600 "the management cluster's API as the snapshot left it" dns01_set false
expect 200 "the ClusterClasses after the restore" request "${work}/admin.jar" "${ui}/api/classes"
body_is '[.[].name] | contains(["kubehenge-kubeadm", "kubehenge-rke2", "kubehenge-k3s"])' "the restored management cluster lost its ClusterClasses"

step "the management cluster's etcd backups and the appliance's upgrades from the appliance's shell"
expect 403 "the etcd backups for a user without privileges" request "${work}/user.jar" "${backups}"
refusal=$(jq -r .error "${work}/body")
cli "${user}" etcd-backups
cli_says false "${refusal}" "the shell showed the backups to a user without privileges, or not in the interface's words"
cli "${admin}" --json etcd-backups
cli_says true '"target":"e2e"' "the shell does not show the backups as set up"
cli "${admin}" etcd-backups snapshots
cli_says true "${second}" "the shell does not list ${second}"
cli "${admin}" etcd-backups take
cli_says true "Taken: " "the shell's snapshot was not taken"
taken=$(sed -n 's/^Taken: //p' "${work}/cli")
until_true 60 "the shell's snapshot in the backups" snapshot_taken "${second}"
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the interface does not show the shell's snapshot" --arg n "${taken}" '.last.name == $n'
cli "${admin}" upgrades
cli_says true "${first_version}" "the shell does not show the installed release"

step "automatic upgrades of the management cluster, switched on and off"
# Their hour half a day away, so that none starts during the test.
hour=$((($(date -u +%H | sed 's/^0//') + 12) % 24))
body=$(jq -n --argjson h "${hour}" '{policy: "off", hour: $h, timeZone: "UTC"}')
expect 200 "the upgrade hour" put "${work}/admin.jar" "${admin_csrf}" "${ui}/api/appliance/upgrades/settings" "${body}"
body=$(jq -n --arg p "not-${passphrase}" '{passphrase: $p}')
expect 403 "switching them on with a wrong passphrase" put "${work}/admin.jar" "${admin_csrf}" "${ui}/api/appliance/upgrades/management" "${body}"
upgrader_is absent "a wrong passphrase made ${upgrader}"
on=$(jq -n --arg p "${passphrase}" '{passphrase: $p}')
expect 200 "switching them on" put "${work}/admin.jar" "${admin_csrf}" "${ui}/api/appliance/upgrades/management" "${on}"
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "they are not on" --arg u "${upgrader}" '.on and .user == $u'
upgrader_is present "switching them on made no ${upgrader}"
expect 200 "switching them off" delete "${work}/admin.jar" "${admin_csrf}" "${ui}/api/appliance/upgrades/management"
body_is '.on | not' "they are still on"
upgrader_is absent "switching them off left ${upgrader}"
# Left on, so that removing the management cluster has to take them away.
expect 200 "switching them on again" put "${work}/admin.jar" "${admin_csrf}" "${ui}/api/appliance/upgrades/management" "${on}"
# The passphrase sealed, with its key on a volume Proxmox VE's backups leave out.
if pct exec "${ctid}" -- cat /var/lib/kubehenge/unattended.json | grep -qF "${passphrase}"; then
    fail "the appliance keeps the passphrase in the open"
fi
pct exec "${ctid}" -- test -s /var/lib/kubehenge-keys/unattended.key || fail "the appliance keeps no key for what automatic upgrades keep"
pct config "${ctid}" | grep -E '^mp[0-9]+: .*mp=/var/lib/kubehenge-keys(,|$)' | grep -Eq '(,|^)backup=0(,|$)' ||
    fail "the key's volume is not left out of backups"

step "the appliance's updater: a release that does not start rolled back, then one that does"
# The appliance trusts the S3 store's CA and reads the test's index; the restart applies both.
pct push "${ctid}" "${work}/s3/ca.crt" /usr/local/share/ca-certificates/kubehenge-e2e-s3.crt --perms 0644
# shellcheck disable=SC2016 # expanded by the container's shell
pct exec "${ctid}" -- sh -c 'update-ca-certificates > /dev/null && echo "KUBEHENGE_RELEASE_INDEX=$1" >> /etc/kubehenge/appliance.env &&
    systemctl restart kubehenge.service' sh "${releases}/index.json"
until_true 120 "the interface to answer again" answers
until_true 60 "the upgrades as the interface shows them" upgrade_status
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the interface does not offer the index's releases" --arg n "${newer_version}" --arg b "${broken_version}" \
    '.available == [$n, $b] and (.error // "") == ""'
expect 202 "the upgrade to the release that does not start" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/appliance/upgrades" \
    "{\"version\":\"${broken_version}\"}"
until_true 600 "the updater to end the upgrade to ${broken_version}" updater_done "${broken_version}"
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the updater did not roll back to ${first_version}, saying why" --arg old "${first_version}" \
    '.status.state == "failed" and .status.rolledBack and .version == $old and
     (.status.error | test("did not start: the interface did not answer within .*; rolled back to \($old)$"))'
runs "${first_version}"
cli "${admin}" upgrades
cli_says true "rolled back to ${first_version}" "the shell does not say the upgrade was rolled back"
cli_says true "Reason" "the shell does not say why the upgrade was rolled back"
cli "${admin}" upgrade "${newer_version}"
cli_says true "Upgrading to ${newer_version}" "the shell did not ask for the upgrade to ${newer_version}"
until_true 600 "the updater to end the upgrade to ${newer_version}" updater_done "${newer_version}"
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the updater did not upgrade to ${newer_version}" --arg v "${newer_version}" \
    '.status.state == "succeeded" and .version == $v and (.status.rolledBack | not)'
runs "${newer_version}"

step "install.sh --upgrade: a release that does not start rolled back, then one that does, with the management cluster up"
upgrade_rolled_back "${broken}" "${newer_version}"
upgrade "${newest}"
if [ "${upgraded}" -ne 0 ]; then
    cat "${work}/upgrade.log" >&2
    fail "install.sh --upgrade failed"
fi
version=${newest_version}
runs "${version}"
sign_in "${work}/admin.jar" "${admin}"
admin_csrf=${csrf}
until_true 120 "the interface to find the management cluster again" ready_again
upgrade_refused "${release}" 'newer than'

step "upgrade the management cluster to what the newest release carries"
old=${first_version}
setup_status
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the interface does not say the management cluster runs what ${old} applied" \
    --arg old "${old}" --arg new "${version}" '.upgrade.behind and .upgrade.applied == $old and .upgrade.carried == $new'
body=$(jq -c --arg p "${passphrase}" '{passphrase: $p, settings: .settings}' "${work}/body")
expect 202 "upgrading the management cluster" post "${work}/admin.jar" "${admin_csrf}" "${ui}/api/setup" "${body}"
until_true 3600 "the management cluster's upgrade to end" job_ended
job_was apply succeeded true
# shellcheck disable=SC2016 # jq's variables, not the shell's.
body_has "the management cluster is not recorded as applied by ${version}" \
    --arg new "${version}" '(.upgrade.behind | not) and .upgrade.applied == $new'
# The apply made the snapshots' talosconfig again, which takes one still.
expect 202 "a snapshot after the upgrade" post "${work}/admin.jar" "${admin_csrf}" "${backups}/snapshots" ''
until_true 300 "the snapshot after the upgrade" snapshot_taken "${second}"

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
expect 200 "the etcd backups after the removal" request "${work}/admin.jar" "${backups}"
body_is '.settings == null and (.keysStored | not)' "the removed management cluster's etcd backups are still set up"

step "remove the appliance"
pct stop "${ctid}"
pct destroy "${ctid}" --purge

step "Proxmox VE as it was"
inventory > "${work}/after"
if ! diff "${work}/before" "${work}/after" >&2; then
    fail "Proxmox VE is not left as it was (< before the test, > after it)"
fi

printf '\nPASS\n'
