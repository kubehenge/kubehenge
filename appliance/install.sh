#!/bin/sh
# Installs Kubehenge's appliance on a Proxmox VE node: a small unprivileged Debian container running
# Kubehenge's web interface, which then sets up the management cluster from the browser.
#
# Run as root in the node's shell:
#
#   sh install.sh [--ctid <id>] [--storage <storage>] [--bridge <bridge>] [--ip dhcp|<address/prefix>]
#                 [--gateway <address>] [--dns <address>,...] [--proxmox-url <URL>] [--proxmox-ca <file>] [--hostname <name>] [--name <DNS name>]
#                 [--cert <file> --key <file>] [--release <file> --sha256 <hex>] [--pve-entry | --no-pve-entry]
#   sh install.sh --upgrade --ctid <id> [--release <file> --sha256 <hex>] [--proxmox-ca <file>]
#   sh install.sh --certificate --ctid <id> [--name <DNS name> | --no-name] [--cert <file> --key <file>]
#
# A new install asks whether to add a Kubernetes entry to this node's Proxmox VE interface, leading
# to the appliance; --pve-entry and --no-pve-entry answer in advance, and without a terminal to ask
# on it is left out. The other nodes get it from share/kubehenge/pve-entry/install.sh in the release.
#
# The appliance's certificate is signed by this Proxmox VE cluster's CA, for its address and, with
# --name, a DNS name users reach it by instead; --cert and --key install one of the administrator's
# own instead. --certificate replaces it on an installed appliance: to renew it, after its address
# changed, to give it a name or an own certificate, or with --no-name to drop the name it was given
# for its address again. --upgrade renews one this script made once it
# has less than 60 days left.
#
# The appliance and the clusters' VMs check the certificate the Proxmox VE API serves against this
# cluster's CA, or with --proxmox-ca against the CA that signed a certificate the administrator put
# in its place; on --upgrade, --proxmox-ca replaces the CA an installed appliance checks it against.
#
# Everything it downloads is checked against a checksum this script carries; the container template
# comes through pveam, which checks it against Proxmox's signed index. --release installs a local
# release archive instead of the published one, with its checksum given. --upgrade replaces the
# software of an installed appliance with a release no older, keeps its state, and waits for no
# run of the management cluster: it stops if one is going on. The management cluster itself is
# then upgraded from the interface.
set -eu

# The release this script installs, and the OpenTofu it runs the management-cluster module with.
KUBEHENGE_VERSION=0.1.0
KUBEHENGE_SHA256=
TOFU_VERSION=1.13.1
TOFU_SHA256=378ada19d4bc70c43732004e8159be771b23b9a5afdf059e5f8a2b3fa2c70a69
TEMPLATE=debian-13-standard_13.6-1_amd64.tar.zst

ctid='' storage='' bridge=vmbr0 ip=dhcp gateway='' dns='' proxmox_url='' proxmox_ca='' hostname=kubehenge release='' release_sha='' upgrade='' pve_entry=''
certificate='' name='' no_name='' cert_file='' key_file=''
while [ $# -gt 0 ]; do
    case "$1" in
        --upgrade)
            upgrade=1
            shift
            continue
            ;;
        --pve-entry)
            pve_entry=yes
            shift
            continue
            ;;
        --no-pve-entry)
            pve_entry=no
            shift
            continue
            ;;
        --certificate)
            certificate=1
            shift
            continue
            ;;
        --no-name)
            no_name=1
            shift
            continue
            ;;
        --name) name=$2 ;;
        --cert) cert_file=$2 ;;
        --key) key_file=$2 ;;
        --ctid) ctid=$2 ;;
        --storage) storage=$2 ;;
        --bridge) bridge=$2 ;;
        --ip) ip=$2 ;;
        --gateway) gateway=$2 ;;
        --dns) dns=$2 ;;
        --proxmox-url) proxmox_url=$2 ;;
        --proxmox-ca) proxmox_ca=$2 ;;
        --hostname) hostname=$2 ;;
        --release) release=$2 ;;
        --sha256) release_sha=$2 ;;
        *)
            echo "unknown option $1" >&2
            exit 2
            ;;
    esac
    shift 2
done

fail() {
    echo "kubehenge: $*" >&2
    exit 1
}
step() { printf '\n== %s\n' "$*"; }
# older <a> <b>: whether version a comes before version b.
older() {
    [ "$1" != "$2" ] || return 1
    first=$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n 1)
    [ "${first}" = "$1" ]
}

uid=$(id -u)
[ "${uid}" -eq 0 ] || fail "run this as root on a Proxmox VE node"
if ! command -v pct > /dev/null || ! command -v pveam > /dev/null; then fail "this is not a Proxmox VE node"; fi
case "$(pveversion)" in pve-manager/9.*) ;; *) fail "Kubehenge needs Proxmox VE 9" ;; esac
if [ -n "${name}" ] && ! printf '%s' "${name}" | grep -Eqx '([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?'; then
    fail "--name is a DNS name such as kubehenge.example.org"
fi
if [ -n "${dns}" ] && ! printf '%s' "${dns}" | grep -Eqx '[0-9A-Fa-f.:]+(,[0-9A-Fa-f.:]+)*'; then
    fail "--dns is a comma-separated list of name server addresses, such as 192.0.2.53,192.0.2.54"
fi
if [ -n "${proxmox_url}" ] && ! printf '%s' "${proxmox_url}" | grep -Eqx 'https://[A-Za-z0-9.:[-]+\]?(:[0-9]+)?'; then
    fail "--proxmox-url is the Proxmox VE API's https:// URL, such as https://pve.example.org:8006"
fi
if [ -n "${proxmox_ca}" ] && ! openssl x509 -in "${proxmox_ca}" -noout 2> /dev/null; then
    fail "--proxmox-ca names a readable PEM certificate: the CA that signed the Proxmox VE API's certificate"
fi
if [ -n "${no_name}" ] && { [ -z "${certificate}" ] || [ -n "${name}" ]; }; then
    fail "--no-name goes with --certificate and without --name"
fi
if [ -n "${cert_file}${key_file}" ] && { [ ! -r "${cert_file}" ] || [ ! -r "${key_file}" ]; }; then
    fail "--cert and --key name a readable PEM certificate and its key"
fi

# appliance_ip: the appliance container's IPv4 address.
appliance_ip() {
    pct exec "${ctid}" -- sh -c "ip -4 -o addr show dev eth0 | sed -n 's/.* inet \([0-9.]*\).*/\1/p'" 2> /dev/null | head -n 1
}

# container_hostname: the host name of the appliance container.
container_hostname() {
    pct config "${ctid}" | sed -n 's/^hostname: //p'
}

# make_certificate <host name> <address> [<DNS name>]: a certificate and key for the appliance in
# $work, signed by this cluster's CA, so that browsers trusting Proxmox VE's certificates trust
# Kubehenge's too. The CA's key stays on this node. 825 days is the longest validity browsers accept
# from a private CA.
make_certificate() {
    printf 'subjectAltName = DNS:%s, IP:%s%s\nextendedKeyUsage = serverAuth\n' "$1" "$2" "${3:+, DNS:$3}" > "${work}/san.cnf"
    openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=${3:-$1}" \
        -keyout "${work}/tls.key" -out "${work}/tls.csr" 2> /dev/null
    if [ -r /etc/pve/priv/pve-root-ca.key ]; then
        openssl x509 -req -in "${work}/tls.csr" -CA /etc/pve/pve-root-ca.pem -CAkey /etc/pve/priv/pve-root-ca.key \
            -CAcreateserial -CAserial "${work}/ca.srl" -days 825 -sha256 -extfile "${work}/san.cnf" -out "${work}/tls.crt" 2> /dev/null
    else
        openssl x509 -req -in "${work}/tls.csr" -signkey "${work}/tls.key" -days 825 -sha256 -extfile "${work}/san.cnf" -out "${work}/tls.crt" 2> /dev/null
    fi
}

# own_certificate <public host>: checks the administrator's --cert and --key and stages them in $work.
own_certificate() {
    openssl x509 -in "${cert_file}" -noout -checkend 86400 > /dev/null 2>&1 ||
        fail "${cert_file} is not a PEM certificate valid for at least another day"
    # A key openssl cannot read gives no public key, whose hash matches no certificate's.
    cert_public_key=$(openssl x509 -in "${cert_file}" -noout -pubkey | sha256sum)
    key_public_key=$(openssl pkey -in "${key_file}" -pubout 2> /dev/null | sha256sum)
    [ "${cert_public_key}" = "${key_public_key}" ] || fail "${key_file} is not the key of ${cert_file}"
    case "$1" in
        *[!0-9.]*) check=-checkhost ;;
        *) check=-checkip ;;
    esac
    openssl x509 -in "${cert_file}" -noout "${check}" "$1" | grep -q 'does match' || fail "${cert_file} is not valid for $1"
    cp "${cert_file}" "${work}/tls.crt"
    cp "${key_file}" "${work}/tls.key"
}

# made_here <certificate>: whether this script made it, signed by this cluster's CA or by itself.
made_here() {
    openssl verify -no_check_time -CAfile /etc/pve/pve-root-ca.pem "$1" > /dev/null 2>&1 && return 0
    issuer=$(openssl x509 -in "$1" -noout -issuer)
    subject=$(openssl x509 -in "$1" -noout -subject | cut -d= -f2-)
    [ "${issuer}" = "issuer=${subject}" ]
}

# public_name: the DNS name an installed appliance is reached by, empty when it is its address.
public_name() {
    host=$(pct exec "${ctid}" -- sed -n 's|^KUBEHENGE_PUBLIC_URL=https://||p' /etc/kubehenge/appliance.env 2> /dev/null | head -n 1)
    case "${host}" in
        *[!0-9.]*) printf '%s' "${host}" ;;
        # An address, or nothing: no name.
        *) ;;
    esac
}

# install_certificate <public host>: puts the certificate staged in $work into an installed
# appliance, and points the interface and Headlamp at the address users reach them by. Both read
# them when they start.
install_certificate() {
    pct push "${ctid}" "${work}/tls.crt" /etc/kubehenge/tls.crt --perms 0644
    pct push "${ctid}" "${work}/tls.key" /etc/kubehenge/tls.key --perms 0600
    # shellcheck disable=SC2016 # expanded by the container's shell
    pct exec "${ctid}" -- sh -c '
        set -e
        chown kubehenge:kubehenge /etc/kubehenge/tls.key
        env=/etc/kubehenge/appliance.env
        { grep -v "^KUBEHENGE_PUBLIC_URL=" "$env" || true; echo "KUBEHENGE_PUBLIC_URL=https://$1"; } > "$env.new"
        chmod 0644 "$env.new" && mv "$env.new" "$env"
        echo "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://$1:8443/oidc-callback" > /etc/kubehenge/headlamp.env
    ' sh "$1"
}

# certificate_report: what users check the certificate against.
certificate_report() {
    fingerprint=$(openssl x509 -in "${work}/tls.crt" -noout -fingerprint -sha256 | cut -d= -f2)
    valid_until=$(openssl x509 -in "${work}/tls.crt" -noout -enddate | cut -d= -f2)
    printf 'Certificate SHA-256 fingerprint: %s\nValid until: %s\n' "${fingerprint}" "${valid_until}"
}

# api_trusted <CA file>: whether the Proxmox VE API at $proxmox_url serves a certificate that CA
# signed for the name or address in the URL, which is what the appliance and the clusters' VMs check.
api_trusted() {
    authority=${proxmox_url#https://}
    case "${authority}" in
        \[*)
            host=${authority#\[}
            host=${host%%\]*}
            port=${authority##*\]}
            target="[${host}]"
            ;;
        *)
            host=${authority%%:*}
            port=${authority#"${host}"}
            target=${host}
            ;;
    esac
    port=${port#:}
    target=${target}:${port:-443}
    case "${host}" in
        *:*) ;;
        *[!0-9.]*)
            timeout 15 openssl s_client -connect "${target}" -servername "${host}" -CAfile "$1" -verify_return_error \
                -verify_hostname "${host}" < /dev/null > /dev/null 2>&1
            return
            ;;
        *) ;;
    esac
    timeout 15 openssl s_client -connect "${target}" -CAfile "$1" -verify_return_error -verify_ip "${host}" < /dev/null > /dev/null 2>&1
}

# check_api <CA file>: stops unless the API's certificate is one that CA signed for the URL.
check_api() {
    # shellcheck disable=SC2310 # api_trusted is a predicate; its status is the answer.
    api_trusted "$1" && return
    if [ -n "${proxmox_ca}" ]; then
        fail "the Proxmox VE API at ${proxmox_url} serves no certificate that ${proxmox_ca} signed for it"
    fi
    fail "the Proxmox VE API at ${proxmox_url} serves a certificate this cluster's CA did not sign for it; give the CA that did with --proxmox-ca <file>, or another URL with --proxmox-url"
}

# stage_api_ca <CA file>: pushes the CA the appliance checks the API against beside its places, its
# own settings and the container's trust store (which OpenTofu's Proxmox VE provider goes by), for
# swap to put in.
stage_api_ca() {
    pct push "${ctid}" "$1" /etc/kubehenge/proxmox-ca.pem.new --perms 0644
    pct push "${ctid}" "$1" /usr/local/share/ca-certificates/proxmox-ca.crt.new --perms 0644
}

if [ -n "${certificate}" ]; then
    [ -n "${ctid}" ] || fail "--certificate needs --ctid"
    pct config "${ctid}" 2> /dev/null | grep -q '^tags:.*kubehenge' || fail "container ${ctid} is not Kubehenge's appliance"
    pct status "${ctid}" | grep -q running || pct start "${ctid}"
    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    ctip=$(appliance_ip)
    [ -n "${ctip}" ] || fail "container ${ctid} has no IPv4 address"
    [ -n "${name}${no_name}" ] || name=$(public_name)
    public=${name:-${ctip}}
    if [ -n "${cert_file}" ]; then
        own_certificate "${public}"
    else
        ct_hostname=$(container_hostname)
        make_certificate "${ct_hostname}" "${ctip}" "${name}"
    fi
    step "certificate for ${public}"
    install_certificate "${public}"
    # Not while the management cluster is applied or removed: the restart would cut the run off.
    rc=0
    # The next visit starts Headlamp with the new settings.
    pct exec "${ctid}" -- flock -n -E 75 /var/lib/kubehenge sh -c 'systemctl restart kubehenge.service && systemctl stop kubehenge-headlamp.service' || rc=$?
    [ "${rc}" -ne 75 ] || fail "the certificate is in place but the interface is busy applying or removing the management cluster; run this again once that has finished"
    [ "${rc}" -eq 0 ] || fail "restarting the interface failed; see: pct exec ${ctid} -- journalctl -u kubehenge"
    printf '\nKubehenge in container %s: https://%s/\n' "${ctid}" "${public}"
    certificate_report
    exit 0
fi

if [ -n "${release}" ]; then
    [ -n "${release_sha}" ] || fail "--release needs --sha256"
    KUBEHENGE_SHA256=${release_sha}
fi
[ -n "${KUBEHENGE_SHA256}" ] || fail "this script carries no release checksum; use --release <file> --sha256 <hex>"

node=$(hostname)
if [ -n "${upgrade}" ]; then
    [ -n "${ctid}" ] || fail "--upgrade needs --ctid"
    pct config "${ctid}" 2> /dev/null | grep -q '^tags:.*kubehenge' || fail "container ${ctid} is not Kubehenge's appliance"
else
    [ -n "${ctid}" ] || ctid=$(pvesh get /cluster/nextid)
    if [ -z "${storage}" ]; then
        # The first active storage that takes container disks.
        storage=$(pvesm status --content rootdir --enabled 1 2> /dev/null | awk 'NR > 1 && $3 == "active" { print $1; exit }')
        [ -n "${storage}" ] || fail "no active storage here takes container disks (content rootdir); name one with --storage"
    fi
    case "${ip}" in
        dhcp) net="name=eth0,bridge=${bridge},ip=dhcp" ;;
        */*) net="name=eth0,bridge=${bridge},ip=${ip}${gateway:+,gw=${gateway}}" ;;
        *) fail "--ip is dhcp or an address with its prefix length, such as 192.0.2.5/24" ;;
    esac
    # The entry changes Proxmox VE's own interface, so it goes in only on the administrator's word,
    # asked before the long steps rather than after them.
    if [ -z "${pve_entry}" ]; then
        pve_entry=no
        if [ -t 0 ]; then
            printf "Add a Kubernetes button to this node's Proxmox VE interface, opening the appliance? [y/N] "
            read -r answer || answer=''
            case "${answer}" in
                [Yy] | [Yy][Ee][Ss]) pve_entry=yes ;;
                *) ;;
            esac
        fi
    fi
fi

work=$(mktemp -d)
created=''
# A failed install leaves no half-made container behind, so running it again starts clean.
cleanup() {
    status=$?
    if [ "${status}" -ne 0 ] && [ -n "${created}" ]; then
        echo "kubehenge: removing container ${ctid} after the failure" >&2
        pct stop "${ctid}" > /dev/null 2>&1 || true
        pct destroy "${ctid}" --purge > /dev/null 2>&1 || true
    fi
    rm -rf "${work}"
}
trap cleanup EXIT

# fetch <url> <file> <sha256>: downloads and checks a file.
fetch() {
    curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"
    echo "$3  $2" | sha256sum -c --quiet - || fail "$1 does not match its checksum"
}

if [ -n "${release}" ]; then
    step "checking ${release} and downloading OpenTofu ${TOFU_VERSION}"
    cp "${release}" "${work}/kubehenge.tar.gz"
    echo "${KUBEHENGE_SHA256}  ${work}/kubehenge.tar.gz" | sha256sum -c --quiet - || fail "${release} does not match its checksum"
else
    step "downloading and checking Kubehenge ${KUBEHENGE_VERSION} and OpenTofu ${TOFU_VERSION}"
    fetch "https://github.com/kubehenge/kubehenge/releases/download/v${KUBEHENGE_VERSION}/kubehenge_${KUBEHENGE_VERSION}_linux_amd64.tar.gz" \
        "${work}/kubehenge.tar.gz" "${KUBEHENGE_SHA256}"
fi
fetch "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_amd64.tar.gz" \
    "${work}/tofu.tar.gz" "${TOFU_SHA256}"
mkdir "${work}/kubehenge" "${work}/tofu"
tar -xzf "${work}/kubehenge.tar.gz" -C "${work}/kubehenge"
tar -xzf "${work}/tofu.tar.gz" -C "${work}/tofu" tofu
# A release archive names its version, which a --release file need not share with this script.
if [ -f "${work}/kubehenge/share/kubehenge/VERSION" ]; then
    KUBEHENGE_VERSION=$(head -c 64 "${work}/kubehenge/share/kubehenge/VERSION" | tr -cd '0-9A-Za-z.+-')
fi

# stage_software pushes the binaries, the module and the service units into the container beside
# their places; swap, run inside it, renames them over the old ones (a running binary cannot be
# overwritten) and exchanges the module directory whole.
stage_software() {
    pct exec "${ctid}" -- sh -c 'mkdir -p /etc/kubehenge /usr/local/share/kubehenge /usr/local/share/ca-certificates'
    pct push "${ctid}" "${work}/kubehenge/bin/kubehenge" /usr/local/bin/kubehenge.new --perms 0755
    [ -z "${push_tofu}" ] || pct push "${ctid}" "${work}/tofu/tofu" /usr/local/bin/tofu.new --perms 0755
    (cd "${work}/kubehenge/share/kubehenge" && tar -cf "${work}/share.tar" .)
    pct push "${ctid}" "${work}/share.tar" /tmp/share.tar
    for unit in ${units}; do
        pct push "${ctid}" "${work}/kubehenge/share/kubehenge/appliance/${unit}" "/etc/systemd/system/${unit}.new" --perms 0644
    done
}
units='kubehenge.service kubehenge-update.service kubehenge-update.path kubehenge-headlamp.service kubehenge-headlamp.socket kubehenge-headlamp-proxy.service'
push_tofu=1
swap="
    set -e
    rm -rf /usr/local/share/kubehenge.new && mkdir /usr/local/share/kubehenge.new
    tar -xf /tmp/share.tar -C /usr/local/share/kubehenge.new && rm /tmp/share.tar
    rm -rf /usr/local/share/kubehenge.old
    mv /usr/local/share/kubehenge /usr/local/share/kubehenge.old
    mv /usr/local/share/kubehenge.new /usr/local/share/kubehenge
    rm -rf /usr/local/share/kubehenge.old
    mv /usr/local/bin/kubehenge.new /usr/local/bin/kubehenge
    [ ! -e /usr/local/bin/tofu.new ] || mv /usr/local/bin/tofu.new /usr/local/bin/tofu
    for unit in ${units}; do mv /etc/systemd/system/\$unit.new /etc/systemd/system/\$unit; done
    if [ -e /etc/kubehenge/proxmox-ca.pem.new ]; then
        mv /etc/kubehenge/proxmox-ca.pem.new /etc/kubehenge/proxmox-ca.pem
        mv /usr/local/share/ca-certificates/proxmox-ca.crt.new /usr/local/share/ca-certificates/proxmox-ca.crt
        update-ca-certificates > /dev/null
    fi
    systemctl daemon-reload
"

if [ -n "${upgrade}" ]; then
    step "upgrading container ${ctid} to Kubehenge ${KUBEHENGE_VERSION}"
    pct status "${ctid}" | grep -q running || pct start "${ctid}"
    installed=$(pct exec "${ctid}" -- cat /usr/local/share/kubehenge/VERSION 2> /dev/null | tr -cd '0-9A-Za-z.+-')
    # shellcheck disable=SC2310 # older is a predicate; its status is the answer.
    if [ -n "${installed}" ] && older "${KUBEHENGE_VERSION}" "${installed}"; then
        fail "container ${ctid} runs Kubehenge ${installed}, newer than ${KUBEHENGE_VERSION}"
    fi
    # The appliance's own upgrades may have brought a newer OpenTofu, and an older one cannot read
    # the state a newer one wrote.
    installed_tofu=$(pct exec "${ctid}" -- /usr/local/bin/tofu version 2> /dev/null | sed -n '1s/^OpenTofu v//p')
    # shellcheck disable=SC2310 # older is a predicate; its status is the answer.
    if [ -n "${installed_tofu}" ] && older "${TOFU_VERSION}" "${installed_tofu}"; then
        echo "keeping OpenTofu ${installed_tofu}, newer than this script's ${TOFU_VERSION}"
        push_tofu=''
    fi
    if [ -n "${proxmox_ca}" ]; then
        [ -n "${proxmox_url}" ] || proxmox_url=$(pct exec "${ctid}" -- sed -n 's/^KUBEHENGE_PROXMOX_URL=//p' /etc/kubehenge/appliance.env)
        check_api "${proxmox_ca}"
    fi
    stage_software
    [ -z "${proxmox_ca}" ] || stage_api_ca "${proxmox_ca}"
    # A certificate this script made is renewed once it has less than 60 days left; the restart
    # below puts it into use.
    pct exec "${ctid}" -- cat /etc/kubehenge/tls.crt > "${work}/current.crt"
    if ! openssl x509 -in "${work}/current.crt" -noout -checkend 5184000 > /dev/null; then
        valid_until=$(openssl x509 -in "${work}/current.crt" -noout -enddate | cut -d= -f2)
        # shellcheck disable=SC2310 # made_here is a predicate; its status is the answer.
        if made_here "${work}/current.crt"; then
            step "renewing the certificate, valid until ${valid_until}"
            name=$(public_name)
            ctip=$(appliance_ip)
            [ -n "${ctip}" ] || fail "container ${ctid} has no IPv4 address"
            ct_hostname=$(container_hostname)
            make_certificate "${ct_hostname}" "${ctip}" "${name}"
            install_certificate "${name:-${ctip}}"
        else
            echo "kubehenge: the appliance's own certificate is valid until ${valid_until}; install a new one with: sh install.sh --certificate --ctid ${ctid} --cert <file> --key <file>" >&2
        fi
    fi
    # Under the interface's job lock, which its own updater takes too: restarting the service
    # during a run of the management-cluster module would cut the run off.
    rc=0
    # shellcheck disable=SC2016 # expanded by the container's shell
    pct exec "${ctid}" -- flock -n -E 75 /var/lib/kubehenge sh -c "${swap}"'
        install -d -o root -g root -m 0755 /var/lib/kubehenge-update
        install -d -o root -g kubehenge -m 0770 /var/lib/kubehenge-update/requests
        grep -q "^KUBEHENGE_APPLIANCE_VMID=" /etc/kubehenge/appliance.env ||
            echo "KUBEHENGE_APPLIANCE_VMID=$1" >> /etc/kubehenge/appliance.env
        ip=$(ip -4 -o addr show dev eth0 | sed -n "s/.* inet \([0-9.]*\).*/\1/p" | head -n 1)
        grep -q "^KUBEHENGE_PUBLIC_URL=" /etc/kubehenge/appliance.env ||
            echo "KUBEHENGE_PUBLIC_URL=https://$ip" >> /etc/kubehenge/appliance.env
        [ -f /etc/kubehenge/headlamp.env ] ||
            echo "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://$ip:8443/oidc-callback" > /etc/kubehenge/headlamp.env
        systemctl enable --now kubehenge-update.path >/dev/null 2>&1
        systemctl restart kubehenge.service
        # The next visit starts the new Headlamp.
        systemctl stop kubehenge-headlamp.service
    ' sh "${ctid}" || rc=$?
    if [ "${rc}" -eq 75 ]; then
        pct exec "${ctid}" -- sh -c 'rm -rf /usr/local/bin/kubehenge.new /usr/local/bin/tofu.new /tmp/share.tar /etc/systemd/system/*.new \
            /etc/kubehenge/proxmox-ca.pem.new /usr/local/share/ca-certificates/proxmox-ca.crt.new'
        fail "the management cluster is being applied or removed, or the appliance upgraded, in container ${ctid}; run this again once that has finished"
    fi
    [ "${rc}" -eq 0 ] || fail "upgrading container ${ctid} failed"
    sleep 2
    pct exec "${ctid}" -- systemctl is-active --quiet kubehenge.service || fail "the service did not start; see: pct exec ${ctid} -- journalctl -u kubehenge"
    printf '\nKubehenge %s is running in container %s. Apply the management cluster again from the interface to upgrade it too.\n' "${KUBEHENGE_VERSION}" "${ctid}"
    exit 0
fi

# The Proxmox VE API as the appliance and the clusters' VMs reach it. Each of them checks the
# certificate the API serves, so the address is one it is valid for: this node's address on the
# container's bridge where it names that, else the first address or name it does name.
if [ -z "${proxmox_url}" ]; then
    served=/etc/pve/local/pveproxy-ssl.pem
    [ -f "${served}" ] || served=/etc/pve/local/pve-ssl.pem
    pveip=$(ip -4 -o addr show dev "${bridge}" | sed -n 's/.* inet \([0-9.]*\).*/\1/p' | head -n 1)
    if [ -n "${pveip}" ] && openssl x509 -in "${served}" -noout -checkip "${pveip}" | grep -q 'does match'; then
        api=${pveip}
    else
        api=$(openssl x509 -in "${served}" -noout -ext subjectAltName | tr ',' '\n' |
            sed -n -e 's/^ *IP Address:\([0-9][0-9.]*\)$/ip \1/p' -e 's/^ *DNS:\(.*\)$/name \1/p' |
            grep -v -e '^ip 127\.' -e '^name localhost$' | sort -s -k 1,1 | head -n 1 | cut -d ' ' -f 2)
        [ -n "${api}" ] || fail "Proxmox VE's certificate names no address of this node; give the API's URL with --proxmox-url"
        echo "kubehenge: Proxmox VE's certificate is not valid for ${pveip:-an address on ${bridge}}; the appliance reaches the API at ${api}, which it names (--proxmox-url gives another)" >&2
    fi
    proxmox_url=https://${api}:8006
fi
api_ca=${proxmox_ca:-/etc/pve/pve-root-ca.pem}
check_api "${api_ca}"

step "container template ${TEMPLATE}"
if ! pveam list local | grep -q "${TEMPLATE}"; then
    pveam update > /dev/null
    pveam download local "${TEMPLATE}"
fi

step "creating container ${ctid} on ${node}"
# Without --nameserver the container is given this node's name servers.
nameservers=$(printf '%s' "${dns}" | tr , ' ')
pct create "${ctid}" "local:vztmpl/${TEMPLATE}" \
    --hostname "${hostname}" --cores 2 --memory 1024 --swap 512 \
    --rootfs "${storage}:8" --net0 "${net}" ${dns:+--nameserver "${nameservers}"} \
    --unprivileged 1 --features nesting=1 --onboot 1 \
    --tags kubehenge --description "Kubehenge's appliance: its web interface and the management cluster's state. Include it in backups."
created=1
pct start "${ctid}"
for _ in $(seq 1 60); do
    # shellcheck disable=SC2310 # the container may not answer yet; an empty address is tried again.
    ctip=$(appliance_ip || true)
    [ -n "${ctip}" ] && break
    sleep 2
done
[ -n "${ctip}" ] || fail "container ${ctid} got no IPv4 address"
public=${name:-${ctip}}

if [ -n "${cert_file}" ]; then
    step "checking the certificate for ${public}"
    own_certificate "${public}"
else
    step "certificate for ${public}, signed by this cluster's CA"
    make_certificate "${hostname}" "${ctip}" "${name}"
fi

step "installing into the container"
stage_software
stage_api_ca "${api_ca}"
pct exec "${ctid}" -- sh -c "${swap}"
pct push "${ctid}" "${work}/tls.crt" /etc/kubehenge/tls.crt --perms 0644
pct push "${ctid}" "${work}/tls.key" /etc/kubehenge/tls.key --perms 0600

printf 'KUBEHENGE_PROXMOX_URL=%s\nKUBEHENGE_PUBLIC_URL=https://%s\nKUBEHENGE_APPLIANCE_VMID=%s\n' "${proxmox_url}" "${public}" "${ctid}" > "${work}/appliance.env"
pct push "${ctid}" "${work}/appliance.env" /etc/kubehenge/appliance.env --perms 0644
printf 'HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://%s:8443/oidc-callback\n' "${public}" > "${work}/headlamp.env"
pct push "${ctid}" "${work}/headlamp.env" /etc/kubehenge/headlamp.env --perms 0644

# Runs inside the container. The appliance is reached only through its interface and pct, so the
# SSH and mail servers of the template are switched off.
# shellcheck disable=SC2016 # expanded by the container's shell
pct exec "${ctid}" -- sh -c '
    set -e
    for unit in ssh.service ssh.socket postfix.service; do
        systemctl disable --now "$unit" >/dev/null 2>&1 || true
        systemctl mask "$unit" >/dev/null 2>&1 || true
    done
    id kubehenge >/dev/null 2>&1 || useradd --system --home-dir /var/lib/kubehenge --shell /usr/sbin/nologin kubehenge
    install -d -o kubehenge -g kubehenge -m 0700 /var/lib/kubehenge
    # The updater (root) reads upgrade requests the interface drops into requests/.
    install -d -o root -g root -m 0755 /var/lib/kubehenge-update
    install -d -o root -g kubehenge -m 0770 /var/lib/kubehenge-update/requests
    chown kubehenge:kubehenge /etc/kubehenge/tls.key
    systemctl daemon-reload
    systemctl enable --now kubehenge-update.path >/dev/null 2>&1
    systemctl enable --now kubehenge.service >/dev/null 2>&1
'
sleep 2
pct exec "${ctid}" -- systemctl is-active --quiet kubehenge.service || fail "the service did not start; see: pct exec ${ctid} -- journalctl -u kubehenge"
# The realms are what the interface asks Proxmox VE before anyone signs in: an answer shows that it
# reaches the API and trusts its certificate. Its own certificate is not the question here.
for _ in $(seq 1 10); do
    realms=$(curl -sk -o /dev/null -w '%{http_code}' "https://${ctip}/api/realms" || true)
    [ "${realms}" != 200 ] || break
    sleep 2
done
[ "${realms}" = 200 ] || fail "the appliance cannot use Proxmox VE's API at ${proxmox_url}; give a URL it reaches, whose certificate names it, with --proxmox-url (see: pct exec ${ctid} -- journalctl -u kubehenge)"

# The appliance works from here on; nothing after this removes it.
created=''

if [ "${pve_entry}" = yes ]; then
    step "Kubernetes entry in this node's Proxmox VE interface"
    if ! sh "${work}/kubehenge/share/kubehenge/pve-entry/install.sh" --url "https://${public}"; then
        echo "kubehenge: the appliance is installed, but adding the entry to this node's Proxmox VE interface failed; see above" >&2
    fi
fi

printf '\nKubehenge is running in container %s: https://%s/\n' "${ctid}" "${public}"
certificate_report
[ -z "${name}" ] || printf 'Point %s at %s in your DNS.\n' "${name}" "${ctip}"
printf 'Sign in with a Proxmox VE administrator to create the management cluster.\n'
