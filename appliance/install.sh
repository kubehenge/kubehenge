#!/bin/sh
# Installs Kubehenge's appliance on a Proxmox VE node: a small unprivileged Debian container running
# Kubehenge's web interface, which then sets up the management cluster from the browser.
#
# Run as root in the node's shell:
#
#   sh install.sh [--ctid <id>] [--storage <storage>] [--template-storage <storage>] [--bridge <bridge>] [--ip dhcp|<address/prefix>]
#                 [--gateway <address>] [--dns <address>,...] [--proxmox-url <URL>] [--proxmox-ca <file> | --proxmox-insecure]
#                 [--hostname <name>] [--name <DNS name>] [--cert <file> --key <file>] [--release <file> --sha256 <hex>]
#                 [--pve-entry | --no-pve-entry]
#   sh install.sh --upgrade --ctid <id> [--release <file> --sha256 <hex>] [--proxmox-ca <file> | --proxmox-insecure]
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
# The appliance and the clusters reach the Proxmox VE API at a name or address of this cluster's
# nodes that the certificate it serves is valid for, asked on the terminal when several are as
# good; --proxmox-url gives another. They check that certificate against this cluster's CA, else
# against the CA in this node's trust store that it rests on, else, once the administrator has
# said yes to its fingerprint on the terminal, against the CA the API serves; --proxmox-ca gives
# the CA (or a bundle holding it) instead, and --proxmox-insecure has them check nothing. On
# --upgrade, --proxmox-ca replaces the CA an installed appliance checks it against and
# --proxmox-insecure stops the checks; the management cluster takes either once it is applied
# again from the interface.
#
# The appliance's disks and the container template go to a storage that takes them, asked on the
# terminal when several do; --storage and --template-storage name them in advance. A template
# already on a storage here is used where it is.
#
# Everything it downloads is checked against a checksum this script carries, the container template
# included, and goes through the HTTP proxy of Proxmox VE's datacenter options unless the
# environment names one. --release installs a local
# release archive instead of the published one, with its checksum given. --upgrade replaces the
# software of an installed appliance with a release no older, keeps its state, and waits for no
# run of the management cluster: it stops if one is going on. Should the new release's interface
# not serve, it puts the old release back and says why. The management cluster itself is then
# upgraded from the interface.
set -eu

# The release this script installs, and the OpenTofu it runs the management-cluster module with.
KUBEHENGE_VERSION=0.1.0
KUBEHENGE_SHA256=
# renovate: datasource=github-release-attachments depName=opentofu/opentofu
TOFU_TAG=v1.13.2
TOFU_SHA256=ac05d9952662c7db28dd57e93f0a68018837bfdf9342384ec0987977c16a9645
TOFU_VERSION=${TOFU_TAG#v}
# The container template and its SHA-512 from Proxmox's template index (aplinfo-pve-9.dat). Proxmox
# serves templates over plain HTTP, so the checksum is what vouches for it. Once the mirror drops
# this point release, the index's signature vouches for the one it lists instead.
TEMPLATE=debian-13-standard_13.6-1_amd64.tar.zst
TEMPLATE_SHA512=4c0c27ca6ceab5ef0b84db57825a00f26157ef1854bafe97297813e1cbe8ecb8cc9c453cab6b3b0efe1ba193a50c47ece1e41d950e411b8730b835b71e9e754b
TEMPLATE_MIRROR=http://download.proxmox.com/images
TEMPLATE_URL=${TEMPLATE_MIRROR}/system/${TEMPLATE}
# The keys Proxmox signs its template index with, as pveam checks it.
template_keyring=/usr/share/doc/pve-manager/trustedkeys.gpg
# This Proxmox VE cluster's CA, this node's trust store, and the terminal questions are asked on,
# which stays the administrator's when the script is piped into sh.
cluster_ca=/etc/pve/pve-root-ca.pem
system_ca=/etc/ssl/certs/ca-certificates.crt
tty=/dev/tty
corosync_conf=/etc/pve/corosync.conf
resolv_conf=/etc/resolv.conf
# The disks need 8 GiB for the root and 1 GiB for the key volume. The working directory holds both
# downloaded archives and what they unpack to, the template storage the 120 MB template.
disks_kib=9437184
work_kib=1048576
template_kib=204800
# The volume holding the key that seals what automatic upgrades keep, which Proxmox VE's backups
# leave out, so that no backup holds both the sealed passphrase and token and what opens them.
key_dir=/var/lib/kubehenge-keys

fail() {
    echo "kubehenge: $*" >&2
    exit 1
}
step() { printf '\n== %s\n' "$*"; }
# ok <what>: a line under the step saying what it finished.
ok() { printf '   %s\n' "$*"; }
# older <a> <b>: whether version a comes before version b.
older() {
    [ "$1" != "$2" ] || return 1
    first=$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n 1)
    [ "${first}" = "$1" ]
}

# appliance_ip: the appliance container's IPv4 address: the one its configuration on this node gives
# it, else the one DHCP gave it, as the container reports it.
appliance_ip() {
    configured=$(pct config "${ctid}" | sed -n 's/^net0: \(.*,\)\{0,1\}ip=\([0-9.]*\)\/.*/\2/p')
    if [ -n "${configured}" ]; then
        printf '%s\n' "${configured}"
        return
    fi
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

# signed_here <certificate>: whether this cluster's CA signed it. openssl verify also trusts its
# default store unless told not to, which would take a certificate from any public CA for one
# made here.
signed_here() {
    openssl verify -no_check_time -no-CApath -no-CAstore -CAfile "${cluster_ca}" "$1" > /dev/null 2>&1
}

# made_here <certificate>: whether this script made it, signed by this cluster's CA or by itself.
made_here() {
    # shellcheck disable=SC2310 # signed_here is a predicate; its status is the answer.
    signed_here "$1" && return 0
    issuer=$(openssl x509 -in "$1" -noout -issuer)
    subject=$(openssl x509 -in "$1" -noout -subject | cut -d= -f2-)
    [ "${issuer}" = "issuer=${subject}" ]
}

# public_name <current certificate> <address>: the DNS name an installed appliance is reached by,
# empty when it is its address. A certificate this cluster's CA signed carries the name the
# administrator gave on this node, beside the host name (make_certificate). A name from anywhere
# else comes from inside the container, whose root can write any, so it is taken only where this
# node's DNS resolves it to the appliance's address: else this cluster's CA would vouch for
# whatever name the container chose.
public_name() {
    # shellcheck disable=SC2310 # signed_here is a predicate; its status is the answer.
    if signed_here "$1"; then
        certificate_names "$1" | sed -n 's/^dns //p' | sed -n 2p
        return
    fi
    host=$(pct exec "${ctid}" -- sed -n 's|^KUBEHENGE_PUBLIC_URL=https://||p' /etc/kubehenge/appliance.env 2> /dev/null | head -n 1)
    case "${host}" in
        *[!0-9.]*) ;;
        # An address, or nothing: no name.
        *) return ;;
    esac
    case "${host}" in
        *[!0-9A-Za-z.-]*) return ;;
        *) ;;
    esac
    if getent ahostsv4 "${host}" 2> /dev/null | awk '{ print $1 }' | grep -qxF "$2"; then
        printf '%s' "${host}"
    else
        echo "kubehenge: leaving ${host} out of the certificate: this node's DNS does not resolve it to $2; give it with --name to keep it" >&2
    fi
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

# interactive: whether there is a terminal to ask on. A plain command, not the special built-in
# ":", fails without stopping the script when it cannot be opened.
interactive() {
    (true < "${tty}") 2> /dev/null
}

# ask <question>: the answer typed on the terminal, empty at its end.
ask() {
    printf '%s ' "$1" >&2
    answer=''
    read -r answer < "${tty}" || true
    printf '%s' "${answer}"
}

# certificate_names <certificate>: the names and IPv4 addresses its subject alternative names
# give, in its order, one a line as "dns <name>" or "ip <address>".
certificate_names() {
    openssl x509 -in "$1" -noout -ext subjectAltName 2> /dev/null | tr ',' '\n' |
        sed -n -e 's/^ *IP Address:\([0-9][0-9.]*\) *$/ip \1/p' -e 's/^ *DNS:\([^ ]*\) *$/dns \1/p'
}

# cluster_addresses: "<address> <node>" a line for each IPv4 address this node knows of this
# Proxmox VE cluster's nodes: its own, and each other node's cluster address and those its name
# resolves to. A node in no cluster is a cluster of one.
cluster_addresses() {
    ip -4 -o addr show | sed -n 's/.* inet \([0-9.]*\).*/\1/p' | sed "s/\$/ ${node}/"
    pvesh get /cluster/status --output-format json 2> /dev/null | tr '{' '\n' |
        grep '"type" *: *"node"' | grep -v '"local" *: *1' | while read -r member; do
        member_name=$(printf '%s' "${member}" | sed -n 's/.*"name" *: *"\([^"]*\)".*/\1/p')
        member_ip=$(printf '%s' "${member}" | sed -n 's/.*"ip" *: *"\([^"]*\)".*/\1/p')
        [ -n "${member_name}" ] || continue
        [ -z "${member_ip}" ] || echo "${member_ip} ${member_name}"
        getent ahostsv4 "${member_name}" 2> /dev/null | awk -v node="${member_name}" '{ print $1, node }'
    done
}

# address_node <address>: the node of $members with that address, empty for none.
address_node() {
    printf '%s\n' "${members}" | awk -v address="$1" '$1 == address { print $2; exit }'
}

# name_nodes <name>: the nodes of $members the name resolves to, one a line, "-" standing for an
# address that is none of theirs; nothing when it does not resolve.
name_nodes() {
    getent ahostsv4 "$1" 2> /dev/null | awk '{ print $1 }' | sort -u | while read -r address; do
        owner=$(address_node "${address}")
        echo "${owner:--}"
    done | sort -u
}

# api_candidates <certificate> <bridge address> <FQDN>: "<rank> <name or address> <what it is>" a
# line for each name and address in the certificate that is this cluster's, best first: a name of
# several of its nodes, this node's own name, another name of it, its address on the bridge,
# another address of it, a name without a domain, then another node's names and addresses. A name
# that also leads to a host outside the cluster is none of them, and of the names a wildcard
# covers only this node's own is known here.
api_candidates() {
    certificate_names "$1" | while read -r kind value; do
        case "${kind}" in
            ip)
                owner=$(address_node "${value}")
                if [ -z "${owner}" ]; then
                    continue
                elif [ "${owner}" != "${node}" ]; then
                    echo "8 ${value} an address of node ${owner}"
                elif [ "${value}" = "$2" ]; then
                    echo "4 ${value} this node's address on ${bridge}"
                else
                    echo "5 ${value} an address of this node"
                fi
                ;;
            *)
                value=$(printf '%s' "${value}" | tr '[:upper:]' '[:lower:]')
                case "${value}" in
                    \*.*)
                        case "$3" in
                            *.*) [ "${3#*.}" = "${value#\*.}" ] || continue ;;
                            *) continue ;;
                        esac
                        value=$3
                        ;;
                    *) ;;
                esac
                owners=$(name_nodes "${value}")
                if [ -z "${owners}" ] || printf '%s\n' "${owners}" | grep -qx -- -; then
                    continue
                fi
                count=$(printf '%s\n' "${owners}" | wc -l)
                case "${value}" in
                    # A name without a domain resolves only where a search domain or a hosts file
                    # supplies one, which the clusters' VMs may well lack: after this node's addresses.
                    *.*) ;;
                    *)
                        if [ "${owners}" = "${node}" ]; then
                            echo "6 ${value} this node's name without a domain"
                        else
                            echo "9 ${value} a name without a domain"
                        fi
                        continue
                        ;;
                esac
                if [ "${count}" -gt 1 ]; then
                    list=$(printf '%s\n' "${owners}" | paste -sd , - | sed 's/,/, /g')
                    echo "1 ${value} a name of nodes ${list}"
                elif [ "${owners}" != "${node}" ]; then
                    echo "7 ${value} a name of node ${owners}"
                elif [ "${value}" = "$3" ]; then
                    echo "2 ${value} this node's name"
                else
                    echo "3 ${value} a name of this node"
                fi
                ;;
        esac
    done | sort -s -n -k 1,1 | awk '!seen[$2]++'
}

# choose_api_host <certificate> <bridge address> <FQDN>: the best of api_candidates, which the
# administrator picks on the terminal when more than one is as good; nothing when there are none.
choose_api_host() {
    candidates=$(api_candidates "$@")
    [ -n "${candidates}" ] || return 0
    best=$(printf '%s\n' "${candidates}" | head -n 1 | cut -d ' ' -f 2)
    top=$(printf '%s\n' "${candidates}" | head -n 1 | cut -d ' ' -f 1)
    tied=$(printf '%s\n' "${candidates}" | awk -v rank="${top}" '$1 == rank' | wc -l)
    count=$(printf '%s\n' "${candidates}" | wc -l)
    # shellcheck disable=SC2310 # interactive is a predicate; its status is the answer.
    if [ "${tied}" -gt 1 ] && interactive; then
        printf "\nProxmox VE's certificate is valid for more than one name or address of this cluster:\n" >&2
        printf '%s\n' "${candidates}" | awk '{ v = $2; $1 = ""; $2 = ""; printf "  %d) %s: %s\n", NR, v, substr($0, 3) }' >&2
        # One reading of the terminal for every try, so that each answer is the next line typed.
        while :; do
            printf 'Which one do the appliance and the clusters reach the Proxmox VE API at? [1] ' >&2
            choice=''
            read -r choice || true
            choice=${choice:-1}
            case "${choice}" in
                *[!0-9]*) ;;
                *) [ "${choice}" -lt 1 ] || [ "${choice}" -gt "${count}" ] || break ;;
            esac
        done < "${tty}"
        printf '%s\n' "${candidates}" | sed -n "${choice}p" | cut -d ' ' -f 2
        return
    fi
    if [ "${count}" -gt 1 ]; then
        others=$(printf '%s\n' "${candidates}" | sed 1d | cut -d ' ' -f 2 | paste -sd , - | sed 's/,/, /g')
        echo "kubehenge: the appliance and the clusters reach the Proxmox VE API at ${best}; its certificate is also valid for ${others} (--proxmox-url gives another)" >&2
    else
        echo "kubehenge: the appliance and the clusters reach the Proxmox VE API at ${best} (--proxmox-url gives another)" >&2
    fi
    echo "${best}"
}

# storages <content> [<least free KiB>]: "<name> <type> <free KiB>" a line for each enabled, active
# storage on this node that takes the content and has that much free, best first: local block
# storage, then directories, NFS, CIFS, CephFS and the rest, and within a kind the most free space.
# A share is slower and goes away with its network, which the appliance's state should not.
storages() {
    pvesm status --content "$1" --enabled 1 2> /dev/null | awk -v least="${2:-0}" '
        NR > 1 && $3 == "active" && $6 >= least {
            rank = 6
            if ($2 == "zfspool" || $2 == "lvmthin" || $2 == "lvm" || $2 == "rbd") rank = 1
            else if ($2 == "dir" || $2 == "btrfs") rank = 2
            else if ($2 == "nfs") rank = 3
            else if ($2 == "cifs") rank = 4
            else if ($2 == "cephfs") rank = 5
            print rank, $1, $2, $6
        }' | sort -s -k 1,1n -k 4,4nr | cut -d ' ' -f 2-
}

# storages_short <content> <least free KiB>: "<name> (<GiB> GiB free), ..." for each storage that
# takes the content but has less free than that.
storages_short() {
    pvesm status --content "$1" --enabled 1 2> /dev/null |
        awk -v least="$2" 'NR > 1 && $3 == "active" && $6 < least { printf "%s%s (%.1f GiB free)", sep, $1, $6 / 1048576; sep = ", " }'
}

# takes <storage> <content> [<least free KiB>]: whether the storage is enabled and active here, takes
# the content and has that much free. The name is matched as written, dots included.
takes() {
    storages "$2" "${3:-0}" | cut -d ' ' -f 1 | grep -qxF -- "$1"
}

# choose_storage <content> <what goes there> <option> [<least free KiB>]: the storage for it. That
# is the only one that takes it, else the one the administrator picks on the terminal, else the
# best ranked (see storages). Nothing when none takes it.
choose_storage() {
    candidates=$(storages "$1" "${4:-0}")
    [ -n "${candidates}" ] || return 0
    best=$(printf '%s\n' "${candidates}" | head -n 1 | cut -d ' ' -f 1)
    count=$(printf '%s\n' "${candidates}" | wc -l)
    # shellcheck disable=SC2310 # interactive is a predicate; its status is the answer.
    if [ "${count}" -gt 1 ] && interactive; then
        printf '\nStorages here for %s:\n' "$2" >&2
        printf '%s\n' "${candidates}" | awk '{ printf "  %d) %s (%s, %.1f GiB free)\n", NR, $1, $2, $3 / 1048576 }' >&2
        # One reading of the terminal for every try, so that each answer is the next line typed.
        while :; do
            printf 'Which one for %s? [1] ' "$2" >&2
            choice=''
            read -r choice || true
            choice=${choice:-1}
            case "${choice}" in
                *[!0-9]*) ;;
                *) [ "${choice}" -lt 1 ] || [ "${choice}" -gt "${count}" ] || break ;;
            esac
        done < "${tty}"
        printf '%s\n' "${candidates}" | sed -n "${choice}p" | cut -d ' ' -f 1
        return
    fi
    best_type=$(printf '%s\n' "${candidates}" | head -n 1 | cut -d ' ' -f 2)
    if [ "${count}" -gt 1 ]; then
        echo "kubehenge: storage ${best} (${best_type}) for $2, the best of ${count} by type and free space ($3 names another)" >&2
    else
        echo "kubehenge: storage ${best} (${best_type}) for $2, the only one here that takes it ($3 names another)" >&2
    fi
    echo "${best}"
}

# template_held: the storage here that already holds the container template, if one does.
template_held() {
    for held in $(storages vztmpl | cut -d ' ' -f 1); do
        if pvesm list "${held}" --content vztmpl 2> /dev/null | grep -q "vztmpl/${TEMPLATE} "; then
            echo "${held}"
            return
        fi
    done
}

# api_endpoint: sets $api_host to the name or address in $proxmox_url, $api_connect to where
# openssl connects for it, and $api_check to the openssl option checking a certificate for it.
api_endpoint() {
    authority=${proxmox_url#https://}
    case "${authority}" in
        \[*)
            api_host=${authority#\[}
            api_host=${api_host%%\]*}
            port=${authority##*\]}
            api_connect="[${api_host}]"
            ;;
        *)
            api_host=${authority%%:*}
            port=${authority#"${api_host}"}
            api_connect=${api_host}
            ;;
    esac
    port=${port#:}
    api_connect=${api_connect}:${port:-443}
    case "${api_host}" in
        *:*) api_check=-verify_ip ;;
        *[!0-9.]*) api_check=-verify_hostname ;;
        *) api_check=-verify_ip ;;
    esac
}

# api_handshake <openssl s_client options>...: a TLS handshake with the API at $proxmox_url, its
# report on stdout. A name goes out as SNI too, as every client of the API sends it.
api_handshake() {
    api_endpoint
    [ "${api_check}" = -verify_ip ] || set -- -servername "${api_host}" "$@"
    timeout 15 openssl s_client -connect "${api_connect}" "$@" < /dev/null 2> /dev/null
}

# api_trusted <CA file>: whether the API at $proxmox_url serves a certificate valid for the name or
# address in the URL that rests on a certificate in that file, and on nothing else openssl would
# trust by default. Every certificate in the file is a trust anchor, as in Go, which the
# appliance, OpenTofu's and Cluster API's Proxmox VE providers, the cloud controller manager and
# the CSI plugin check the certificate with; openssl alone would want a self-signed root.
api_trusted() {
    api_endpoint
    api_handshake -CAfile "$1" -no-CApath -no-CAstore -partial_chain -verify_return_error "${api_check}" "${api_host}" > /dev/null
}

# served_chain <directory>: writes the certificates the API serves into the directory as 1.pem,
# 2.pem and on, in the order it serves them, its own first.
served_chain() {
    rm -rf "$1"
    mkdir "$1"
    api_handshake -showcerts | awk -v dir="$1" '
        /-----BEGIN CERTIFICATE-----/ { n++; inside = 1 }
        inside { print > (dir "/" n ".pem") }
        /-----END CERTIFICATE-----/ { inside = 0; close(dir "/" n ".pem") }'
    [ -f "$1/1.pem" ]
}

# chain_authorities <CA file> <output>: writes to <output> the certificates of the CA file that the
# chain the API serves is verified through, up to a root where the file holds one, so that a
# bundle such as a trust store comes down to the CA the API's certificate rests on.
chain_authorities() {
    ca_file=$1 authorities=$2
    chain=${work}/chain
    # shellcheck disable=SC2310 # served_chain is a predicate; its status is the answer.
    served_chain "${chain}" || return 1
    : > "${chain}/sent.pem"
    for sent in "${chain}"/[0-9]*.pem; do
        [ "${sent}" = "${chain}/1.pem" ] || cat "${sent}" >> "${chain}/sent.pem"
    done
    set -- -no-CApath -no-CAstore -CAfile "${ca_file}" -show_chain
    [ ! -s "${chain}/sent.pem" ] || set -- "$@" -untrusted "${chain}/sent.pem"
    verified=$(openssl verify "$@" "${chain}/1.pem" 2> /dev/null ||
        openssl verify "$@" -partial_chain "${chain}/1.pem" 2> /dev/null) || return 1
    anchors=$(printf '%s\n' "${verified}" | sed -n 's/^depth=[0-9]*: //p' | grep -v ' (untrusted)$') || return 1
    # The topmost alone where the API serves the rest of the chain, else all the file gave it.
    anchor=$(printf '%s\n' "${anchors}" | tail -n 1)
    certificates_named "${ca_file}" "${anchor}" > "${authorities}"
    # shellcheck disable=SC2310 # api_trusted is a predicate; its status is the answer.
    api_trusted "${authorities}" || certificates_named "${ca_file}" "${anchors}" > "${authorities}"
    grep -q -e '-----BEGIN CERTIFICATE-----' "${authorities}"
}

# certificates_named <PEM file> <subjects>: the certificates in the file whose subject is one of
# the lines given, as openssl prints subjects.
certificates_named() {
    openssl crl2pkcs7 -nocrl -certfile "$1" | openssl pkcs7 -print_certs |
        subjects=$2 awk '
            /^subject=/ { keep = index("\n" ENVIRON["subjects"] "\n", "\n" substr($0, 9) "\n") > 0 }
            /-----BEGIN CERTIFICATE-----/ { inside = keep }
            inside { print }
            /-----END CERTIFICATE-----/ { inside = 0 }'
}

# served_authority <output>: writes to <output> the certificate to trust of those the API serves:
# the self-signed root it serves, else the topmost CA certificate it serves, else its own.
served_authority() {
    served_dir=${work}/served
    # shellcheck disable=SC2310 # served_chain is a predicate; its status is the answer.
    served_chain "${served_dir}" || return 1
    served_root='' served_top=''
    i=2
    while [ -f "${served_dir}/${i}.pem" ]; do
        if openssl x509 -in "${served_dir}/${i}.pem" -noout -ext basicConstraints 2> /dev/null | grep -q 'CA:TRUE'; then
            served_top=${served_dir}/${i}.pem
        fi
        if openssl verify -no-CApath -no-CAstore -check_ss_sig -CAfile "${served_dir}/${i}.pem" "${served_dir}/${i}.pem" > /dev/null 2>&1; then
            served_root=${served_dir}/${i}.pem
        fi
        i=$((i + 1))
    done
    cp "${served_root:-${served_top:-${served_dir}/1.pem}}" "$1"
}

# api_connects: whether the API at $proxmox_url answers a TLS handshake with a certificate, whatever
# that certificate is.
api_connects() {
    api_endpoint
    api_handshake | grep -q -e '-----BEGIN CERTIFICATE-----'
}

# check_api <CA file>: stops unless the API's certificate is one that CA signed for the URL.
check_api() {
    # shellcheck disable=SC2310 # api_trusted is a predicate; its status is the answer.
    api_trusted "$1" && return
    # shellcheck disable=SC2310 # api_connects is a predicate; its status is the answer.
    if ! api_connects; then
        fail "this node cannot connect to the Proxmox VE API at ${proxmox_url}. Check that port ${api_connect##*:} is open in the node's firewall and that pveproxy listens on it (LISTEN_IP in /etc/default/pveproxy), or give another URL with --proxmox-url"
    fi
    if [ -n "${proxmox_ca}" ]; then
        fail "the Proxmox VE API at ${proxmox_url} serves no certificate that ${proxmox_ca} signed for it"
    fi
    fail "the Proxmox VE API at ${proxmox_url} serves a certificate this cluster's CA did not sign for it; give the CA that did with --proxmox-ca <file>, or another URL with --proxmox-url"
}

# choose_api_ca: sets $api_ca to the CA file the appliance and the clusters check the API's
# certificate against, or stops: the certificates of --proxmox-ca the API's chain is verified
# through; else this cluster's CA; else the CA in this node's trust store the chain rests on; else
# the CA the API serves, once the administrator has said yes to it on the terminal.
choose_api_ca() {
    api_ca=${work}/proxmox-ca.pem
    if [ -n "${proxmox_ca}" ]; then
        check_api "${proxmox_ca}"
        # shellcheck disable=SC2310 # a file the chain cannot be traced in verified whole, so it goes whole.
        chain_authorities "${proxmox_ca}" "${api_ca}" || cp "${proxmox_ca}" "${api_ca}"
        return
    fi
    # shellcheck disable=SC2310 # api_trusted is a predicate; its status is the answer.
    if api_trusted "${cluster_ca}"; then
        api_ca=${cluster_ca}
        return
    fi
    # shellcheck disable=SC2310 # api_trusted and chain_authorities are predicates.
    if [ -r "${system_ca}" ] && api_trusted "${system_ca}" && chain_authorities "${system_ca}" "${api_ca}"; then
        subject=$(openssl x509 -in "${api_ca}" -noout -subject | sed 's/^subject=//')
        echo "kubehenge: the Proxmox VE API's certificate rests on ${subject} in this node's trust store; the appliance and the clusters check it against that CA (--proxmox-ca gives another)" >&2
        return
    fi
    # shellcheck disable=SC2310 # served_authority and api_trusted are predicates.
    if served_authority "${api_ca}" && api_trusted "${api_ca}"; then
        subject=$(openssl x509 -in "${api_ca}" -noout -subject | sed 's/^subject=//')
        issuer=$(openssl x509 -in "${api_ca}" -noout -issuer | sed 's/^issuer=//')
        fingerprint=$(openssl x509 -in "${api_ca}" -noout -fingerprint -sha256 | cut -d= -f2)
        # shellcheck disable=SC2310 # interactive is a predicate; its status is the answer.
        if ! interactive; then
            fail "the Proxmox VE API at ${proxmox_url} serves a certificate neither this cluster's CA nor this node's trust store vouches for, resting on ${subject} with the SHA-256 fingerprint ${fingerprint}; once you have checked that this is your CA, give it with --proxmox-ca <file>"
        fi
        printf "\nThe Proxmox VE API at %s serves a certificate neither this cluster's CA nor this node's trust store vouches for. It rests on:\n  Subject: %s\n  Issuer: %s\n  SHA-256 fingerprint: %s\n" \
            "${proxmox_url}" "${subject}" "${issuer}" "${fingerprint}" >&2
        answer=$(ask "Trust this CA for the Proxmox VE API? [y/N]")
        case "${answer}" in
            [Yy] | [Yy][Ee][Ss]) return ;;
            *) ;;
        esac
    fi
    check_api "${cluster_ca}"
}

# stage_api_ca <CA file>: pushes the CA the appliance checks the API against beside its places, its
# own settings and the container's trust store (which OpenTofu's Proxmox VE provider goes by), for
# swap to put in.
stage_api_ca() {
    pct push "${ctid}" "$1" /etc/kubehenge/proxmox-ca.pem.new --perms 0644
    pct push "${ctid}" "$1" /usr/local/share/ca-certificates/proxmox-ca.crt.new --perms 0644
}

units='kubehenge.service kubehenge-update.service kubehenge-update.path kubehenge-headlamp.service kubehenge-headlamp.socket kubehenge-headlamp-proxy.service'

# An upgrade as it runs in the container, under the interface's job lock, once stage_software
# pushed the new release beside the old: it sets the installed software aside, puts the new in
# place and restarts the interface on it. Should the interface not serve and keep running, the
# check the appliance's own updater makes, it puts the old software back and restarts that.
#   sh -c "${upgrade_script}" sh <root> <container ID> <true|false|""> <units>
# <root> is "" but in the tests; the third argument stops (true) or starts (false) the checks of
# the API's certificate, or leaves them. Exits 76 rolled back, with the reason on standard output,
# and 77 when putting the old software back failed too.
# shellcheck disable=SC2016 # expanded by the container's shell
upgrade_script='
set -eu
R=$1 ctid=$2 insecure=$3 units=$4
aside=$R/usr/local/share/.kubehenge-rollback bin=$R/usr/local/bin share=$R/usr/local/share
env=$R/etc/kubehenge/appliance.env ca=$R/etc/kubehenge/proxmox-ca.pem trusted=$R/usr/local/share/ca-certificates/proxmox-ca.crt
rm -rf "$aside" "$share/kubehenge.new"
mkdir -p "$aside/units" "$share/kubehenge.new"
cp -p "$bin/kubehenge" "$env" "$aside/"
[ ! -e "$bin/tofu.new" ] || cp -p "$bin/tofu" "$aside/"
for unit in $units; do
    [ ! -e "$R/etc/systemd/system/$unit" ] || cp -p "$R/etc/systemd/system/$unit" "$aside/units/"
done
ca_changed=
if [ -e "$ca.new" ]; then
    ca_changed=1
    cp -p "$ca" "$aside/proxmox-ca.pem"
    [ ! -e "$trusted" ] || cp -p "$trusted" "$aside/proxmox-ca.crt"
fi

tar -xf "$R/tmp/share.tar" -C "$share/kubehenge.new" && rm "$R/tmp/share.tar"
mv "$share/kubehenge" "$aside/share"
mv "$share/kubehenge.new" "$share/kubehenge"
mv "$bin/kubehenge.new" "$bin/kubehenge"
[ ! -e "$bin/tofu.new" ] || mv "$bin/tofu.new" "$bin/tofu"
for unit in $units; do mv "$R/etc/systemd/system/$unit.new" "$R/etc/systemd/system/$unit"; done
if [ -n "$ca_changed" ]; then
    mv "$ca.new" "$ca"
    mv "$trusted.new" "$trusted"
    update-ca-certificates > /dev/null
fi
install -d -o root -g root -m 0755 "$R/var/lib/kubehenge-update"
install -d -o root -g kubehenge -m 0770 "$R/var/lib/kubehenge-update/requests"
install -d -o kubehenge -g kubehenge -m 0700 "$R/var/lib/kubehenge-keys"
grep -q "^KUBEHENGE_APPLIANCE_VMID=" "$env" || echo "KUBEHENGE_APPLIANCE_VMID=$ctid" >> "$env"
ip=$(ip -4 -o addr show dev eth0 | sed -n "s/.* inet \([0-9.]*\).*/\1/p" | head -n 1)
grep -q "^KUBEHENGE_PUBLIC_URL=" "$env" || echo "KUBEHENGE_PUBLIC_URL=https://$ip" >> "$env"
if [ -n "$insecure" ]; then
    { grep -v "^KUBEHENGE_PROXMOX_INSECURE=" "$env" || true; [ "$insecure" = false ] || echo "KUBEHENGE_PROXMOX_INSECURE=true"; } > "$env.new"
    chmod 0644 "$env.new" && mv "$env.new" "$env"
fi
[ -f "$R/etc/kubehenge/headlamp.env" ] ||
    echo "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://$ip:8443/oidc-callback" > "$R/etc/kubehenge/headlamp.env"
systemctl daemon-reload
systemctl enable --now kubehenge-update.path > /dev/null 2>&1
systemctl restart kubehenge.service
# The next visit starts the new Headlamp.
systemctl stop kubehenge-headlamp.service

if reason=$("$bin/kubehenge" appliance health --cert "$R/etc/kubehenge/tls.crt" 2>&1); then
    rm -rf "$aside"
    exit 0
fi
printf "%s\n" "${reason#kubehenge: }"

# Each step is checked by hand: set -e does not hold inside a function called as a condition.
roll_back() {
    cp -p "$aside/kubehenge" "$bin/kubehenge.old" && mv -f "$bin/kubehenge.old" "$bin/kubehenge" || return 1
    if [ -e "$aside/tofu" ]; then
        cp -p "$aside/tofu" "$bin/tofu.old" && mv -f "$bin/tofu.old" "$bin/tofu" || return 1
    fi
    rm -rf "$share/kubehenge" && mv "$aside/share" "$share/kubehenge" || return 1
    for unit in $units; do
        if [ -e "$aside/units/$unit" ]; then
            cp -p "$aside/units/$unit" "$R/etc/systemd/system/$unit" || return 1
        else
            rm -f "$R/etc/systemd/system/$unit" || return 1
        fi
    done
    cp -p "$aside/appliance.env" "$env" || return 1
    if [ -n "$ca_changed" ]; then
        cp -p "$aside/proxmox-ca.pem" "$ca" || return 1
        if [ -e "$aside/proxmox-ca.crt" ]; then
            cp -p "$aside/proxmox-ca.crt" "$trusted" || return 1
        else
            rm -f "$trusted" || return 1
        fi
        update-ca-certificates > /dev/null || return 1
    fi
    systemctl daemon-reload && systemctl restart kubehenge.service || return 1
    systemctl stop kubehenge-headlamp.service || return 1
    rm -rf "$aside"
}
if roll_back && sleep 5 && systemctl is-active --quiet kubehenge.service; then
    exit 76
fi
exit 77
'

# proxy_label <proxy URL>: scheme://host:port of it, without credentials or path, as safe to print.
proxy_label() {
    case "$1" in
        *://*)
            label_scheme=${1%%://*}
            label_rest=${1#*://}
            ;;
        *)
            label_scheme=http
            label_rest=$1
            ;;
    esac
    label_rest=${label_rest##*@}
    printf '%s://%s' "${label_scheme}" "${label_rest%%/*}"
}

# read_download_proxy <datacenter options file>: sets $download_proxy to the HTTP proxy of Proxmox
# VE's datacenter options, empty when the environment names one (curl reads that itself) or none is
# set. It is passed to fetch alone: exported, it would send the requests to the container and to
# the Proxmox VE API through the proxy too.
read_download_proxy() {
    download_proxy=''
    [ -z "${https_proxy:-}${HTTPS_PROXY:-}${http_proxy:-}" ] || return 0
    download_proxy=$(sed -n 's/^http_proxy:[[:space:]]*\(.*[^[:space:]]\)[[:space:]]*$/\1/p' "$1" 2> /dev/null | head -n 1)
    [ -n "${download_proxy}" ] || return 0
    shown=$(proxy_label "${download_proxy}")
    echo "kubehenge: downloading through ${shown}, the HTTP proxy of the datacenter options"
}

# appliance_realms <public host> <address> <CA file>: the HTTP status of the appliance's realms
# list. The container is on this node's network, so the datacenter's or the environment's proxy
# never sees the request, which it could not route to a private address.
appliance_realms() {
    curl -s --noproxy '*' --cacert "$3" --resolve "$1:443:$2" -o /dev/null -w '%{http_code}' "https://$1/api/realms" || true
}

# fetch <url> <file> <SHA-256 or SHA-512> <what it is> [<protocols>]: downloads and checks a file, with
# a progress bar on a terminal. Any failed transfer is tried again, a connection reset mid-way
# included, which --retry alone leaves out. HTTPS only, unless the protocols say otherwise.
fetch() {
    progress=-s
    [ ! -t 2 ] || progress=--progress-bar
    ok "downloading $4"
    # shellcheck disable=SC2086 # the unquoted expansion is two words on purpose, the URL one of them.
    rc=0
    http=$(curl -fSL ${download_proxy:+--proxy "${download_proxy}"} "${progress}" --proto "${5:-=https}" --retry 8 --retry-all-errors --tlsv1.2 -w '%{http_code}' -o "$2" "$1") || rc=$?
    if [ "${rc}" -ne 0 ]; then
        # 000 is curl's word for no HTTP answer at all.
        [ "${http}" != 000 ] || http=''
        fail "could not download $1 (curl exit ${rc}${http:+, HTTP status ${http}})"
    fi
    case "${#3}" in
        128) sum=sha512sum ;;
        *) sum=sha256sum ;;
    esac
    echo "$3  $2" | "${sum}" -c --quiet - || fail "$1 does not match its checksum"
    ok "$4: checksum verified"
}

# template_gone: whether Proxmox's mirror answers 404 for the pinned template. Any other answer,
# no answer included, leaves the pin to fetch and its own error.
template_gone() {
    # shellcheck disable=SC2086 # the unquoted expansion is two words on purpose, the URL one of them.
    [ "$(curl -sI ${download_proxy:+--proxy "${download_proxy}"} --proto '=http,https' --retry 3 -o /dev/null -w '%{http_code}' "${TEMPLATE_URL}" || true)" = 404 ]
}

# template_from_index <directory>: the newest amd64 Debian 13 standard template in Proxmox's template
# index, as "<file> <SHA-512> <location>". The index is checked as pveam checks it, against this
# node's keyring of Proxmox's signing keys.
template_from_index() {
    index=$1/aplinfo-pve-9.dat
    for suffix in .gz .asc; do
        # shellcheck disable=SC2086 # the unquoted expansion is two words on purpose, the URL one of them.
        curl -fsSL ${download_proxy:+--proxy "${download_proxy}"} --proto '=http,https' --retry 8 --retry-all-errors -o "${index}${suffix}" "${TEMPLATE_MIRROR}/aplinfo-pve-9.dat${suffix}" ||
            fail "could not download Proxmox's template index ${TEMPLATE_MIRROR}/aplinfo-pve-9.dat${suffix}"
    done
    gunzip -f "${index}.gz" || fail "Proxmox's template index ${TEMPLATE_MIRROR}/aplinfo-pve-9.dat.gz does not unpack"
    sqv --keyring "${template_keyring}" "${index}.asc" "${index}" > /dev/null 2>&1 ||
        fail "Proxmox's template index ${TEMPLATE_MIRROR}/aplinfo-pve-9.dat fails its signature check against ${template_keyring}"
    found=$(awk '
        function entry() {
            if (pkg == "debian-13-standard" && arch == "amd64" && loc ~ /^system\/[A-Za-z0-9._+-]+$/ && sum ~ /^[0-9a-f]{128}$/) print ver, loc, sum
            pkg = arch = ver = loc = sum = ""
        }
        /^$/ { entry(); next }
        $1 == "Package:" { pkg = $2 }
        $1 == "Architecture:" { arch = $2 }
        $1 == "Version:" { ver = $2 }
        $1 == "Location:" { loc = $2 }
        $1 == "sha512sum:" { sum = $2 }
        END { entry() }
    ' "${index}" | sort -V | tail -n 1)
    [ -n "${found}" ] || fail "Proxmox's template index lists no amd64 Debian 13 standard template"
    read -r _ location sha << EOF
${found}
EOF
    echo "${location##*/} ${sha} ${location}"
}

# local_archive <file> <SHA-256> <destination> <what it is>: copies a file the administrator gave
# and checks it.
local_archive() {
    cp "$1" "$3"
    echo "$2  $3" | sha256sum -c --quiet - || fail "$1 does not match its checksum"
    ok "$4: checksum verified"
}

# need_space <directory> <KiB> <what needs it> <what to do>: stops unless the file system holding the
# directory has that much free.
need_space() {
    free_kib=$(df -Pk "$1" 2> /dev/null | awk 'NR == 2 { print $4 }')
    [ -z "${free_kib}" ] || [ "${free_kib}" -ge "$2" ] ||
        fail "$1 has $((free_kib / 1024)) MiB free and $3 needs $(($2 / 1024)) MiB. $4"
}

# is_ipv4 <address>: whether it is a dotted IPv4 address.
is_ipv4() {
    printf '%s' "$1" | grep -Eqx '([0-9]{1,3}\.){3}[0-9]{1,3}' || return 1
    for octet in $(printf '%s' "$1" | tr . ' '); do
        [ "${octet}" -le 255 ] || return 1
    done
}

# is_ipv4_cidr <address/prefix>: whether it is an IPv4 address with a prefix length of 1 to 32.
is_ipv4_cidr() {
    case "$1" in
        */*) ;;
        *) return 1 ;;
    esac
    # shellcheck disable=SC2310 # is_ipv4 is a predicate; its status is the answer.
    is_ipv4 "${1%/*}" || return 1
    in_range "${1#*/}" 1 32
}

# in_range <value> <least> <most>: whether the value is a whole number between them.
in_range() {
    case "$1" in
        '' | *[!0-9]*) return 1 ;;
        *) ;;
    esac
    [ "${#1}" -le 6 ] && [ "$1" -ge "$2" ] && [ "$1" -le "$3" ]
}

# container_net: the container's net0 setting, from $bridge, $vlan, $mtu, $ip and $gateway.
container_net() {
    printf 'name=eth0,bridge=%s%s%s,ip=%s' "${bridge}" "${vlan:+,tag=${vlan}}" "${mtu:+,mtu=${mtu}}" "${ip}"
    [ "${ip}" = dhcp ] || printf '%s' "${gateway:+,gw=${gateway}}"
}

# node_bridges: the bridges, Open vSwitch bridges and SDN VNets a container on this node can attach
# to, one a line.
node_bridges() {
    { pvesh get "/nodes/${node}/network" --output-format json 2> /dev/null | tr '{' '\n' |
        grep -E '"type" *: *"(bridge|OVSBridge)"' | sed -n 's/.*"iface" *: *"\([^"]*\)".*/\1/p' || true; } | sort -u
    # Where SDN is not set up, the call fails and there are none.
    pvesh get /cluster/sdn/vnets --output-format json 2> /dev/null | tr '{' '\n' |
        sed -n 's/.*"vnet" *: *"\([^"]*\)".*/\1/p' | sort -u || true
}

# bridge_exists <bridge>: whether it is one of node_bridges.
bridge_exists() {
    node_bridges | grep -qxF -- "$1"
}

# default_bridge: the bridge holding this node's default route, else vmbr0 where it exists. A node
# whose management address sits on a bond or a VLAN interface has neither, and fails.
default_bridge() {
    route_dev=$(ip -4 route show default 2> /dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -n 1)
    # shellcheck disable=SC2310 # bridge_exists is a predicate; its status is the answer.
    if [ -n "${route_dev}" ] && bridge_exists "${route_dev}"; then
        printf '%s\n' "${route_dev}"
        return
    fi
    # shellcheck disable=SC2310 # bridge_exists is a predicate; its status is the answer.
    if bridge_exists vmbr0; then
        echo vmbr0
        return
    fi
    return 1
}

# bridge_list: node_bridges as "a, b", or "none".
bridge_list() {
    listed_bridges=$(node_bridges | paste -sd , - | sed 's/,/, /g')
    printf '%s' "${listed_bridges:-none}"
}

# appliance_containers: "<ID> <node>" for each container of the cluster tagged kubehenge, as a
# whole tag among the ones Proxmox VE separates with semicolons.
appliance_containers() {
    pvesh get /cluster/resources --type vm --output-format json 2> /dev/null | tr '{' '\n' | awk '
        /"type" *: *"lxc"/ && match($0, /"tags" *: *"[^"]*"/) {
            tags = substr($0, RSTART, RLENGTH)
            sub(/^"tags" *: *"/, "", tags)
            sub(/"$/, "", tags)
            found = 0
            n = split(tags, parts, ";")
            for (i = 1; i <= n; i++) if (parts[i] == "kubehenge") found = 1
            if (!found || !match($0, /"vmid" *: *[0-9]+/)) next
            id = substr($0, RSTART, RLENGTH)
            sub(/.*: */, "", id)
            if (!match($0, /"node" *: *"[^"]*"/)) next
            host = substr($0, RSTART, RLENGTH)
            sub(/^"node" *: *"/, "", host)
            sub(/"$/, "", host)
            print id, host
        }'
}

# refuse_second_appliance: stops when a container of the cluster is already an appliance.
refuse_second_appliance() {
    existing=$(appliance_containers | head -n 1)
    [ -z "${existing}" ] || fail "an appliance exists as container ${existing% *} on ${existing#* }. Run this with --upgrade on that node."
}

# require_appliance: stops unless container $ctid is an appliance on this node.
require_appliance() {
    where=$(appliance_containers | awk -v id="${ctid}" '$1 == id { print $2; exit }')
    if [ -n "${where}" ] && [ "${where}" != "${node}" ]; then
        fail "container ${ctid} runs on node ${where}. Run this there."
    fi
    [ -n "${where}" ] && return 0
    # The cluster's list may fail where this node's own configuration answers.
    pct config "${ctid}" 2> /dev/null | sed -n 's/^tags: *//p' | tr ';' '\n' | grep -qxF kubehenge ||
        fail "container ${ctid} is not Kubehenge's appliance"
}

# want_pve_entry: yes or no, asked on the terminal the other questions use.
want_pve_entry() {
    # shellcheck disable=SC2310 # interactive is a predicate; its status is the answer.
    if ! interactive; then
        echo no
        return
    fi
    reply=$(ask "Add a Kubernetes button to this node's Proxmox VE interface, opening the appliance? [y/N]")
    case "${reply}" in
        [Yy] | [Yy][Ee][Ss]) echo yes ;;
        *) echo no ;;
    esac
}

# install_traps <command>: runs the command when the script ends, and also when the session drops
# or is interrupted, which would otherwise leave the container and the files half made.
install_traps() {
    # shellcheck disable=SC2064 # the command is the argument as given.
    trap "$1" EXIT
    trap 'exit 1' HUP INT TERM
}

# cluster_quorate: whether this node can change the cluster's configuration. A node in no cluster
# always can.
cluster_quorate() {
    quorum_report=$(pvecm status 2> /dev/null) || quorum_report=''
    if printf '%s\n' "${quorum_report}" | grep -Eq '^Quorate:[[:space:]]+Yes'; then
        return 0
    fi
    [ -z "${quorum_report}" ] && [ ! -e "${corosync_conf}" ]
}

# loopback_resolver_only: whether the node's resolver file names name servers, all on loopback.
loopback_resolver_only() {
    resolvers=$(sed -n 's/^nameserver[[:space:]]\{1,\}\([^[:space:]]\{1,\}\).*/\1/p' "${resolv_conf}" 2> /dev/null)
    [ -n "${resolvers}" ] && ! printf '%s\n' "${resolvers}" | grep -qvE '^(127\.|::1$)'
}

# ordered_api_hosts <chosen> <certificate> <bridge address> <FQDN>: the host chosen, then the other
# candidates of api_candidates in rank order, one a line.
ordered_api_hosts() {
    {
        echo "$1"
        shift
        api_candidates "$@" | cut -d ' ' -f 2
    } | awk 'NF && !seen[$0]++'
}

# resolvable_api_host <hosts, one a line>: the first that is an address, or a name the container
# resolves. The node's own hosts file may be all that knows a node's name.
resolvable_api_host() {
    for candidate in $1; do
        case "${candidate}" in
            *[!0-9.]*)
                pct exec "${ctid}" -- getent hosts "${candidate}" > /dev/null 2>&1 || continue
                ;;
            *) ;;
        esac
        echo "${candidate}"
        return 0
    done
    return 1
}

# require_resolvable_api_host <hosts, one a line>: resolvable_api_host, or stops naming the hosts
# tried.
require_resolvable_api_host() {
    # shellcheck disable=SC2310 # resolvable_api_host is a predicate; its status is the answer.
    resolvable_api_host "$1" && return
    tried=$(printf '%s\n' "$1" | paste -sd , - | sed 's/,/, /g')
    fail "the container cannot resolve any host the Proxmox VE API could be reached at (tried ${tried}). Give an address its certificate is valid for with --proxmox-url, or name servers the container reaches with --dns"
}

# container_gateway: the default gateway the container has, or nothing.
container_gateway() {
    pct exec "${ctid}" -- sh -c "ip -4 route show default | sed -n 's/^default via \([0-9.]*\).*/\1/p' | head -n 1" 2> /dev/null | head -n 1
}

# check_container_gateway: stops unless the container's gateway answers a ping. A container without
# a gateway, or without ping, passes: the later steps find what is wrong.
check_container_gateway() {
    # shellcheck disable=SC2310 # the container may have no route; an empty gateway is skipped.
    container_gw=$(container_gateway || true)
    [ -n "${container_gw}" ] || return 0
    ping_rc=0
    # shellcheck disable=SC2016 # expanded by the container's shell
    pct exec "${ctid}" -- sh -c 'command -v ping > /dev/null || exit 127; ping -c 1 -W 3 "$1" > /dev/null' sh "${container_gw}" > /dev/null 2>&1 || ping_rc=$?
    case "${ping_rc}" in
        0 | 127) ;;
        *) fail "container ${ctid} at ${ctip} cannot reach its gateway ${container_gw}. Check that --vlan matches the network of the address, and that --gateway is on it" ;;
    esac
}

# check_container_api_port <host>: stops unless the container connects to the API's port on that
# host. Bash opens the connection, which the template has without anything installed.
check_container_api_port() {
    api_endpoint
    api_port=${api_connect##*:}
    # shellcheck disable=SC2016 # expanded by the container's shell
    if ! pct exec "${ctid}" -- timeout 5 bash -c 'exec 3<>"/dev/tcp/$1/$2"' bash "$1" "${api_port}" > /dev/null 2>&1; then
        no_route=''
        # shellcheck disable=SC2310 # the container may have no route; an empty gateway is the answer.
        [ -n "$(container_gateway || true)" ] || no_route=" The container has no default route, so give --gateway."
        fail "container ${ctid} cannot connect to the Proxmox VE API at $1 on port ${api_port}. Check the node's firewall for that port and pveproxy's LISTEN_IP in /etc/default/pveproxy.${no_route}"
    fi
}

# id_free <id>: whether no guest in the cluster has the id.
id_free() {
    pvesh get /cluster/nextid --vmid "$1" > /dev/null 2>&1
}

# cleanup removes what a failed install made, so running it again starts clean. created is set
# before pct create, so a container whose creation was cut off goes too, locked as pct left it,
# with any disk it had allocated and not yet named in its config.
cleanup() {
    status=$?
    if [ "${status}" -ne 0 ] && [ -n "${created}" ]; then
        # The service's log goes with the container, so it is shown before the container is removed.
        if [ -n "${started}" ]; then
            echo "kubehenge: the service's log:" >&2
            pct exec "${ctid}" -- journalctl -u kubehenge --no-pager -n 60 >&2 2> /dev/null || true
        fi
        echo "kubehenge: removing container ${ctid} after the failure" >&2
        pct unlock "${ctid}" > /dev/null 2>&1 || true
        pct stop "${ctid}" > /dev/null 2>&1 || true
        pct destroy "${ctid}" --purge --destroy-unreferenced-disks 1 > /dev/null 2>&1 || true
    fi
    [ -z "${partial}" ] || rm -f "${partial}"
    rm -rf "${work}"
}

# The tests source this file for its functions alone.
[ -z "${KUBEHENGE_INSTALL_FUNCTIONS:-}" ] || return 0

usage() {
    cat << 'EOF'
Installs Kubehenge's appliance on this Proxmox VE node, as root.

  sh install.sh [options]
  sh install.sh --upgrade --ctid <id> [--release <file> --sha256 <hex>] [--proxmox-ca <file> | --proxmox-insecure]
  sh install.sh --certificate --ctid <id> [--name <DNS name> | --no-name] [--cert <file> --key <file>]

Options:
  --ctid <id>                 the container's ID; default the next free one
  --storage <storage>         where the container's disks go, 9 GiB; default the only storage that takes them, else asked
  --template-storage <storage> where the container template goes; default where it already is, else the only storage that takes it, else asked
  --bridge <bridge>           a bridge, Open vSwitch bridge or SDN VNet the container attaches to; default the bridge of the node's default route, else vmbr0
  --vlan <tag>                the VLAN tag of the container's network, 1 to 4094; default none
  --mtu <bytes>               the MTU of the container's network, 576 to 65535; default the bridge's
  --ip dhcp|<address/prefix>  the container's IPv4 address, such as 192.0.2.40/24; default dhcp
  --gateway <address>         the default gateway, an IPv4 address, with a static --ip
  --dns <address>,...         the name servers the container asks; default this node's
  --hostname <name>           the container's host name, also in its certificate; default kubehenge
  --name <DNS name>           a DNS name people reach the appliance by, put into its certificate
  --no-name                   with --certificate, drop the name the certificate was given
  --cert <file> --key <file>  serve this certificate and key instead of one signed by this cluster's CA
  --proxmox-url <URL>         the Proxmox VE API, such as https://pve.example.org:8006; default the best name or address its certificate is valid for
  --proxmox-ca <file>         the CA that signed the API's certificate, alone or in a bundle
  --proxmox-insecure          reach the Proxmox VE API without verifying its certificate
  --release <file>            install this release archive instead of downloading it
  --sha256 <hex>              the archive's SHA-256, with --release
  --tofu <file>               install OpenTofu from this release archive instead of downloading it, checked against the checksum this script carries
  --pve-entry, --no-pve-entry add the Kubernetes entry to this node's Proxmox VE interface, or not; default ask
  --upgrade                   upgrade the appliance in container --ctid
  --certificate               replace the certificate of the appliance in container --ctid
  -h, --help                  show this text

More: https://kubehenge.dev/docs/install
EOF
}

ctid='' storage='' template_storage='' bridge='' vlan='' mtu='' tofu_archive='' ip=dhcp gateway='' dns='' proxmox_url='' proxmox_ca='' proxmox_insecure='' hostname=kubehenge
release='' release_sha='' upgrade='' pve_entry='' certificate='' name='' no_name='' cert_file='' key_file=''
while [ $# -gt 0 ]; do
    case "$1" in
        -h | --help)
            usage
            exit 0
            ;;
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
        --proxmox-insecure)
            proxmox_insecure=1
            shift
            continue
            ;;
        --name | --cert | --key | --ctid | --storage | --template-storage | --bridge | --vlan | --mtu | --ip | --gateway | --dns | --proxmox-url | --proxmox-ca | --hostname | --release | --sha256 | --tofu)
            [ $# -ge 2 ] || fail "$1 needs a value"
            ;;
        *) ;;
    esac
    case "$1" in
        --name) name=$2 ;;
        --cert) cert_file=$2 ;;
        --key) key_file=$2 ;;
        --ctid) ctid=$2 ;;
        --storage) storage=$2 ;;
        --template-storage) template_storage=$2 ;;
        --bridge) bridge=$2 ;;
        --vlan) vlan=$2 ;;
        --mtu) mtu=$2 ;;
        --tofu) tofu_archive=$2 ;;
        --ip) ip=$2 ;;
        --gateway) gateway=$2 ;;
        --dns) dns=$2 ;;
        --proxmox-url) proxmox_url=$2 ;;
        --proxmox-ca) proxmox_ca=$2 ;;
        --hostname) hostname=$2 ;;
        --release) release=$2 ;;
        --sha256) release_sha=$2 ;;
        *)
            echo "kubehenge: unknown option $1; sh install.sh --help lists them" >&2
            exit 2
            ;;
    esac
    shift 2
done

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
    fail "--proxmox-ca names a readable PEM file of the CA that signed the Proxmox VE API's certificate, alone or among others"
fi
if [ -n "${proxmox_ca}" ] && [ -n "${proxmox_insecure}" ]; then
    fail "--proxmox-ca and --proxmox-insecure exclude each other"
fi
if [ -n "${proxmox_insecure}" ] && [ -n "${certificate}" ]; then
    fail "--proxmox-insecure goes with an install or --upgrade"
fi
# shellcheck disable=SC2310 # is_ipv4_cidr is a predicate; its status is the answer.
if [ "${ip}" != dhcp ] && ! is_ipv4_cidr "${ip}"; then
    fail "--ip is dhcp or an IPv4 address with its prefix length, such as 192.0.2.5/24"
fi
# shellcheck disable=SC2310 # is_ipv4 is a predicate; its status is the answer.
if [ -n "${gateway}" ] && ! is_ipv4 "${gateway}"; then
    fail "--gateway is an IPv4 address, such as 192.0.2.1"
fi
# shellcheck disable=SC2310 # in_range is a predicate; its status is the answer.
if [ -n "${vlan}" ] && ! in_range "${vlan}" 1 4094; then
    fail "--vlan is a VLAN tag from 1 to 4094"
fi
# shellcheck disable=SC2310 # in_range is a predicate; its status is the answer.
if [ -n "${mtu}" ] && ! in_range "${mtu}" 576 65535; then
    fail "--mtu is a number of bytes from 576 to 65535"
fi
if [ -n "${tofu_archive}" ] && [ ! -r "${tofu_archive}" ]; then
    fail "--tofu names a readable OpenTofu release archive"
fi
if [ -n "${no_name}" ] && { [ -z "${certificate}" ] || [ -n "${name}" ]; }; then
    fail "--no-name goes with --certificate and without --name"
fi
if [ -n "${cert_file}${key_file}" ] && { [ ! -r "${cert_file}" ] || [ ! -r "${key_file}" ]; }; then
    fail "--cert and --key name a readable PEM certificate and its key"
fi

node=$(hostname)
# shellcheck disable=SC2310 # cluster_quorate is a predicate; its status is the answer.
cluster_quorate || fail "this node has no quorum in its Proxmox VE cluster, so it cannot change any container. Start the other nodes, then run this again."

if [ -n "${certificate}" ]; then
    [ -n "${ctid}" ] || fail "--certificate needs --ctid"
    require_appliance
    pct status "${ctid}" | grep -q running || pct start "${ctid}"
    work=$(mktemp -d)
    # shellcheck disable=SC2016 # expanded when the trap runs.
    install_traps 'rm -rf "$work"'
    ctip=$(appliance_ip)
    [ -n "${ctip}" ] || fail "container ${ctid} has no IPv4 address"
    if [ -z "${name}${no_name}" ]; then
        pct exec "${ctid}" -- cat /etc/kubehenge/tls.crt > "${work}/current.crt" 2> /dev/null || true
        name=$(public_name "${work}/current.crt" "${ctip}")
    fi
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

if [ -n "${upgrade}" ]; then
    [ -n "${ctid}" ] || fail "--upgrade needs --ctid"
    require_appliance
else
    refuse_second_appliance
    # The bridge and the name servers are checked before the long steps, which cannot help a
    # container that will not start or reach anything.
    bridges_here=$(bridge_list)
    # shellcheck disable=SC2310 # default_bridge and bridge_exists are predicates.
    if [ -z "${bridge}" ]; then
        bridge=$(default_bridge) || fail "no bridge carries this node's default route and there is no vmbr0. Name one of ${bridges_here} with --bridge"
    elif ! bridge_exists "${bridge}"; then
        fail "${bridge} is no bridge, Open vSwitch bridge or SDN VNet of this node. Name one of ${bridges_here} with --bridge"
    fi
    # shellcheck disable=SC2310 # loopback_resolver_only is a predicate; its status is the answer.
    if [ -z "${dns}" ] && loopback_resolver_only; then
        fail "${resolv_conf} names only name servers on this node's loopback, which the container cannot reach. Give the name servers it asks with --dns, such as 192.0.2.53,192.0.2.54"
    fi
    if [ -z "${ctid}" ]; then
        ctid=$(pvesh get /cluster/nextid)
    else
        # shellcheck disable=SC2310 # id_free is a predicate; its status is the answer.
        id_free "${ctid}" || fail "--ctid ${ctid} is taken by another guest. Name a free id, or leave --ctid out"
    fi
    # Asked before the long steps rather than during them.
    if [ -z "${storage}" ]; then
        storage=$(choose_storage rootdir "the appliance's disks" --storage "${disks_kib}")
        if [ -z "${storage}" ]; then
            short=$(storages_short rootdir "${disks_kib}")
            [ -z "${short}" ] || fail "no storage that takes container disks has the 9 GiB free that the appliance needs (8 for its root disk, 1 for its key volume): ${short}. Free space, or name another with --storage"
            fail "no enabled, active storage here takes container disks (content rootdir). Enable one, or name one with --storage"
        fi
    else
        # shellcheck disable=SC2310 # takes is a predicate; its status is the answer.
        takes "${storage}" rootdir "${disks_kib}" || fail "storage ${storage} is disabled, inactive, takes no container disks here or has under 9 GiB free for them. Name another with --storage"
    fi
    if [ -z "${template_storage}" ]; then
        template_storage=$(template_held)
        [ -n "${template_storage}" ] || template_storage=$(choose_storage vztmpl "the container template" --template-storage)
        [ -n "${template_storage}" ] || fail "no enabled, active storage here takes container templates (content vztmpl). Enable one, or name one with --template-storage"
    else
        # shellcheck disable=SC2310 # takes is a predicate; its status is the answer.
        takes "${template_storage}" vztmpl || fail "storage ${template_storage} is disabled, inactive or takes no container templates here. Name another with --template-storage"
    fi
    net=$(container_net)
    # The entry changes Proxmox VE's own interface, so it goes in only on the administrator's word,
    # asked before the long steps rather than after them.
    [ -n "${pve_entry}" ] || pve_entry=$(want_pve_entry)
fi

work=$(mktemp -d)
created=''
started=''
partial=''
install_traps cleanup
need_space "${work}" "${work_kib}" "the downloaded archives and what they unpack to" "Free space there, or set TMPDIR to a directory on a larger file system."

# Downloads go through the proxy Proxmox VE's own downloads use, unless the environment names one.
read_download_proxy /etc/pve/datacenter.cfg

step "getting and checking Kubehenge ${KUBEHENGE_VERSION} and OpenTofu ${TOFU_VERSION}"
if [ -n "${release}" ]; then
    local_archive "${release}" "${KUBEHENGE_SHA256}" "${work}/kubehenge.tar.gz" "${release}"
else
    fetch "https://github.com/kubehenge/kubehenge/releases/download/v${KUBEHENGE_VERSION}/kubehenge_${KUBEHENGE_VERSION}_linux_amd64.tar.gz" \
        "${work}/kubehenge.tar.gz" "${KUBEHENGE_SHA256}" "Kubehenge ${KUBEHENGE_VERSION}"
fi
if [ -n "${tofu_archive}" ]; then
    local_archive "${tofu_archive}" "${TOFU_SHA256}" "${work}/tofu.tar.gz" "${tofu_archive}"
else
    fetch "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_amd64.tar.gz" \
        "${work}/tofu.tar.gz" "${TOFU_SHA256}" "OpenTofu ${TOFU_VERSION}"
fi
mkdir "${work}/kubehenge" "${work}/tofu"
tar -xzf "${work}/kubehenge.tar.gz" -C "${work}/kubehenge"
tar -xzf "${work}/tofu.tar.gz" -C "${work}/tofu" tofu
ok "unpacked"
# A release archive names its version, which a --release file need not share with this script.
if [ -f "${work}/kubehenge/share/kubehenge/VERSION" ]; then
    KUBEHENGE_VERSION=$(head -c 64 "${work}/kubehenge/share/kubehenge/VERSION" | tr -cd '0-9A-Za-z.+-')
fi

# stage_software pushes the binaries, the module and the service units into the container beside
# their places; swap or, upgrading, upgrade_script, run inside it, renames them over the old ones
# (a running binary cannot be overwritten) and exchanges the module directory whole.
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
    # Whether the appliance checks the API's certificate changes only when told: --proxmox-insecure
    # stops the checks, a CA given starts them again.
    insecure=''
    if [ -n "${proxmox_insecure}" ]; then
        insecure=true
    elif [ -n "${proxmox_ca}" ]; then
        insecure=false
        [ -n "${proxmox_url}" ] || proxmox_url=$(pct exec "${ctid}" -- sed -n 's/^KUBEHENGE_PROXMOX_URL=//p' /etc/kubehenge/appliance.env)
        choose_api_ca
    fi
    if ! pct config "${ctid}" | grep -Eq "^mp[0-9]+: .*mp=${key_dir}(,|$)"; then
        step "a volume for the key of automatic upgrades, left out of backups"
        free=0
        while pct config "${ctid}" | grep -q "^mp${free}: "; do free=$((free + 1)); done
        rootfs_storage=$(pct config "${ctid}" | sed -n 's/^rootfs: \([^:]*\):.*/\1/p')
        pct set "${ctid}" "--mp${free}" "${rootfs_storage}:1,mp=${key_dir},backup=0"
    fi
    stage_software
    [ -z "${proxmox_ca}" ] || stage_api_ca "${api_ca}"
    # A certificate this script made is renewed once it has less than 60 days left; the restart
    # below puts it into use.
    pct exec "${ctid}" -- cat /etc/kubehenge/tls.crt > "${work}/current.crt"
    if ! openssl x509 -in "${work}/current.crt" -noout -checkend 5184000 > /dev/null; then
        valid_until=$(openssl x509 -in "${work}/current.crt" -noout -enddate | cut -d= -f2)
        # shellcheck disable=SC2310 # made_here is a predicate; its status is the answer.
        if made_here "${work}/current.crt"; then
            step "renewing the certificate, valid until ${valid_until}"
            ctip=$(appliance_ip)
            [ -n "${ctip}" ] || fail "container ${ctid} has no IPv4 address"
            name=$(public_name "${work}/current.crt" "${ctip}")
            ct_hostname=$(container_hostname)
            make_certificate "${ct_hostname}" "${ctip}" "${name}"
            install_certificate "${name:-${ctip}}"
        else
            echo "kubehenge: the appliance's own certificate is valid until ${valid_until}; install a new one with: sh install.sh --certificate --ctid ${ctid} --cert <file> --key <file>" >&2
        fi
    fi
    # Under the interface's job lock, which its own updater takes too: restarting the service
    # during a run of the management-cluster module would cut the run off.
    step "starting Kubehenge ${KUBEHENGE_VERSION}, and checking that it serves"
    rc=0
    reason=$(pct exec "${ctid}" -- flock -n -E 75 /var/lib/kubehenge sh -c "${upgrade_script}" sh '' "${ctid}" "${insecure}" "${units}") || rc=$?
    before=${installed:-the release installed before}
    case "${rc}" in
        0) ;;
        75)
            pct exec "${ctid}" -- sh -c 'rm -rf /usr/local/bin/kubehenge.new /usr/local/bin/tofu.new /tmp/share.tar /etc/systemd/system/*.new \
                /etc/kubehenge/proxmox-ca.pem.new /usr/local/share/ca-certificates/proxmox-ca.crt.new'
            fail "the management cluster is being applied or removed, or the appliance upgraded, in container ${ctid}; run this again once that has finished"
            ;;
        76) fail "release ${KUBEHENGE_VERSION} did not start: ${reason}; rolled back to ${before}" ;;
        77) fail "release ${KUBEHENGE_VERSION} did not start: ${reason}; rolling back to ${before} failed too; see: pct exec ${ctid} -- journalctl -u kubehenge" ;;
        *) fail "upgrading container ${ctid} failed${reason:+: ${reason}}" ;;
    esac
    printf '\nKubehenge %s is running in container %s. Apply the management cluster again from the interface to upgrade it too.\n' "${KUBEHENGE_VERSION}" "${ctid}"
    exit 0
fi

# The Proxmox VE API as the appliance and the clusters' VMs reach it. Each of them checks the
# certificate the API serves, so the name or address is one it is valid for, and one of this
# cluster's nodes: a certificate may name other hosts too.
if [ -z "${proxmox_url}" ]; then
    served=/etc/pve/local/pveproxy-ssl.pem
    [ -f "${served}" ] || served=/etc/pve/local/pve-ssl.pem
    pveip=$(ip -4 -o addr show dev "${bridge}" | sed -n 's/.* inet \([0-9.]*\).*/\1/p' | head -n 1)
    fqdn=$(hostname -f 2> /dev/null | tr '[:upper:]' '[:lower:]')
    members=$(cluster_addresses | grep -v '^127\.' | sort -u)
    api=$(choose_api_host "${served}" "${pveip}" "${fqdn}")
    if [ -z "${api}" ]; then
        named=$(certificate_names "${served}" | cut -d ' ' -f 2 | paste -sd , - | sed 's/,/, /g')
        fail "Proxmox VE's certificate (${served}) is valid for no name or address of this cluster's nodes, only for ${named:-none}; give the URL the appliance and the clusters reach the API at, by a name or address the certificate is valid for, with --proxmox-url"
    fi
    proxmox_url=https://${api}:8006
    api_hosts=$(ordered_api_hosts "${api}" "${served}" "${pveip}" "${fqdn}")
else
    api_endpoint
    api_hosts=${api_host}
fi
if [ -n "${proxmox_insecure}" ]; then
    api_ca=${cluster_ca}
    echo "kubehenge: the appliance and the clusters reach the Proxmox VE API at ${proxmox_url} without checking its certificate (--proxmox-insecure)" >&2
else
    choose_api_ca
fi

step "container template ${TEMPLATE} on ${template_storage}"
template_file=$(pvesm path "${template_storage}:vztmpl/${TEMPLATE}") || fail "storage ${template_storage} gives no path for the container template. Name another with --template-storage"
template_there() {
    [ -f "${template_file}" ] && echo "${TEMPLATE_SHA512}  ${template_file}" | sha512sum -c --quiet - 2> /dev/null
}
# shellcheck disable=SC2310 # template_there and template_gone are predicates; their status is the answer.
if ! template_there && template_gone; then
    listed=$(template_from_index "${work}") || exit 1
    read -r TEMPLATE TEMPLATE_SHA512 location << EOF
${listed}
EOF
    TEMPLATE_URL=${TEMPLATE_MIRROR}/${location}
    ok "Proxmox's mirror no longer has the pinned template. Its signed index lists ${TEMPLATE}"
    template_file=$(pvesm path "${template_storage}:vztmpl/${TEMPLATE}") || fail "storage ${template_storage} gives no path for the container template. Name another with --template-storage"
fi
# shellcheck disable=SC2310 # template_there is a predicate; its status is the answer.
if template_there; then
    ok "already there, checksum verified"
else
    mkdir -p "$(dirname "${template_file}")"
    need_space "$(dirname "${template_file}")" "${template_kib}" "the container template" "Free space on storage ${template_storage}, or name another with --template-storage."
    # Downloaded beside its place and moved there once checked, so no half or wrong file is left as
    # the template.
    partial=${template_file}.part
    fetch "${TEMPLATE_URL}" "${partial}" "${TEMPLATE_SHA512}" "the container template" '=http,https'
    mv "${partial}" "${template_file}"
    partial=''
fi

step "creating container ${ctid} on ${node}"
# Without --nameserver the container is given this node's name servers.
nameservers=$(printf '%s' "${dns}" | tr , ' ')
# shellcheck disable=SC2310 # id_free is a predicate; its status is the answer.
id_free "${ctid}" || fail "id ${ctid} was taken by another guest while the installer ran. Run it again, or name a free id with --ctid"
created=1
pct create "${ctid}" "${template_storage}:vztmpl/${TEMPLATE}" \
    --hostname "${hostname}" --cores 2 --memory 1024 --swap 512 \
    --rootfs "${storage}:8" --mp0 "${storage}:1,mp=${key_dir},backup=0" \
    --net0 "${net}" ${dns:+--nameserver "${nameservers}"} \
    --unprivileged 1 --features nesting=1 --onboot 1 \
    --tags kubehenge --description "Kubehenge's appliance: its web interface and the management cluster's state. Include it in backups."
pct start "${ctid}"
for _ in $(seq 1 60); do
    # shellcheck disable=SC2310 # the container may not answer yet; an empty address is tried again.
    ctip=$(appliance_ip || true)
    [ -n "${ctip}" ] && break
    sleep 2
done
[ -n "${ctip}" ] || fail "container ${ctid} got no IPv4 address. Check that --vlan is the right one and that a DHCP server answers on it, or give --ip and --gateway"
ok "running, at ${ctip}"
public=${name:-${ctip}}
check_container_gateway

# This node's hosts file may be all that knows a node's name, and the container does not read it.
# shellcheck disable=SC2310 # resolvable_api_host is a predicate; its status is the answer.
reachable=$(require_resolvable_api_host "${api_hosts}")
first_host=$(printf '%s\n' "${api_hosts}" | head -n 1)
if [ "${reachable}" != "${first_host}" ]; then
    echo "kubehenge: the container cannot resolve ${first_host}, so the appliance and the clusters reach the Proxmox VE API at ${reachable}" >&2
    proxmox_url=https://${reachable}:8006
fi
check_container_api_port "${reachable}"

if [ -n "${cert_file}" ]; then
    step "checking the certificate for ${public}"
    own_certificate "${public}"
    ok "it matches its key and is valid for ${public}"
else
    step "certificate for ${public}, signed by this cluster's CA"
    make_certificate "${hostname}" "${ctip}" "${name}"
    ok "made"
fi

step "installing into the container"
stage_software
stage_api_ca "${api_ca}"
pct exec "${ctid}" -- sh -c "${swap}"
pct push "${ctid}" "${work}/tls.crt" /etc/kubehenge/tls.crt --perms 0644
pct push "${ctid}" "${work}/tls.key" /etc/kubehenge/tls.key --perms 0600

printf 'KUBEHENGE_PROXMOX_URL=%s\nKUBEHENGE_PUBLIC_URL=https://%s\nKUBEHENGE_APPLIANCE_VMID=%s\n' "${proxmox_url}" "${public}" "${ctid}" > "${work}/appliance.env"
[ -z "${proxmox_insecure}" ] || echo 'KUBEHENGE_PROXMOX_INSECURE=true' >> "${work}/appliance.env"
pct push "${ctid}" "${work}/appliance.env" /etc/kubehenge/appliance.env --perms 0644
printf 'HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://%s:8443/oidc-callback\n' "${public}" > "${work}/headlamp.env"
pct push "${ctid}" "${work}/headlamp.env" /etc/kubehenge/headlamp.env --perms 0644
ok "software, certificate and settings in place"

step "starting Kubehenge ${KUBEHENGE_VERSION}"

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
    install -d -o kubehenge -g kubehenge -m 0700 /var/lib/kubehenge /var/lib/kubehenge-keys
    # The updater (root) reads upgrade requests the interface drops into requests/.
    install -d -o root -g root -m 0755 /var/lib/kubehenge-update
    install -d -o root -g kubehenge -m 0770 /var/lib/kubehenge-update/requests
    chown kubehenge:kubehenge /etc/kubehenge/tls.key
    systemctl daemon-reload
    systemctl enable --now kubehenge-update.path >/dev/null 2>&1
    systemctl enable --now kubehenge.service >/dev/null 2>&1
'
started=1
sleep 2
pct exec "${ctid}" -- systemctl is-active --quiet kubehenge.service || fail "the service did not start; its log follows"
ok "the service is running"
# The realms are what the interface asks Proxmox VE before anyone signs in: an answer shows that it
# reaches the API and trusts its certificate. The appliance's own certificate is checked as the one
# just installed: curl takes any certificate in --cacert as a trust anchor, the leaf included, so
# this holds for one Proxmox VE's CA signed, a self-signed one and the administrator's own alike.
for _ in $(seq 1 10); do
    realms=$(appliance_realms "${public}" "${ctip}" "${work}/tls.crt")
    [ "${realms}" != 200 ] || break
    sleep 2
done
[ "${realms}" = 200 ] || fail "the appliance cannot use Proxmox VE's API at ${proxmox_url}; give a URL it reaches, whose certificate names it, with --proxmox-url (its log follows)"
ok "it serves its certificate and reaches the Proxmox VE API at ${proxmox_url}"

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
