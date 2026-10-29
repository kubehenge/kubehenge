#!/bin/sh
# Installs Kubehenge's appliance on a Proxmox VE node: a small unprivileged Debian container running
# Kubehenge's web interface, which then sets up the management cluster from the browser.
#
# Run as root in the node's shell:
#
#   sh install.sh [--ctid <id>] [--storage <storage>] [--bridge <bridge>] [--ip dhcp|<address/prefix>]
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
# renovate: datasource=github-release-attachments depName=opentofu/opentofu
TOFU_TAG=v1.13.2
TOFU_SHA256=ac05d9952662c7db28dd57e93f0a68018837bfdf9342384ec0987977c16a9645
TOFU_VERSION=${TOFU_TAG#v}
TEMPLATE=debian-13-standard_13.6-1_amd64.tar.zst
# This Proxmox VE cluster's CA, this node's trust store, and the terminal questions are asked on,
# which stays the administrator's when the script is piped into sh.
cluster_ca=/etc/pve/pve-root-ca.pem
system_ca=/etc/ssl/certs/ca-certificates.crt
tty=/dev/tty

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
    # openssl verify also trusts its default store unless told not to, which would take a
    # certificate from any public CA for one made here.
    openssl verify -no_check_time -no-CApath -no-CAstore -CAfile "${cluster_ca}" "$1" > /dev/null 2>&1 && return 0
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

# check_api <CA file>: stops unless the API's certificate is one that CA signed for the URL.
check_api() {
    # shellcheck disable=SC2310 # api_trusted is a predicate; its status is the answer.
    api_trusted "$1" && return
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
  --storage <storage>         where the container's disk goes; default the first active storage that takes container disks
  --bridge <bridge>           the bridge the container attaches to; default vmbr0
  --ip dhcp|<address/prefix>  the container's address, such as 192.0.2.40/24; default dhcp
  --gateway <address>         the default gateway, with a static --ip
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
  --pve-entry, --no-pve-entry add the Kubernetes entry to this node's Proxmox VE interface, or not; default ask
  --upgrade                   upgrade the appliance in container --ctid
  --certificate               replace the certificate of the appliance in container --ctid
  -h, --help                  show this text

More: https://kubehenge.dev/docs/install
EOF
}

ctid='' storage='' bridge=vmbr0 ip=dhcp gateway='' dns='' proxmox_url='' proxmox_ca='' proxmox_insecure='' hostname=kubehenge
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
if [ -n "${no_name}" ] && { [ -z "${certificate}" ] || [ -n "${name}" ]; }; then
    fail "--no-name goes with --certificate and without --name"
fi
if [ -n "${cert_file}${key_file}" ] && { [ ! -r "${cert_file}" ] || [ ! -r "${key_file}" ]; }; then
    fail "--cert and --key name a readable PEM certificate and its key"
fi

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
started=''
# A failed install leaves no half-made container behind, so running it again starts clean.
cleanup() {
    status=$?
    if [ "${status}" -ne 0 ] && [ -n "${created}" ]; then
        # The service's log goes with the container, so it is shown before the container is removed.
        if [ -n "${started}" ]; then
            echo "kubehenge: the service's log:" >&2
            pct exec "${ctid}" -- journalctl -u kubehenge --no-pager -n 60 >&2 2> /dev/null || true
        fi
        echo "kubehenge: removing container ${ctid} after the failure" >&2
        pct stop "${ctid}" > /dev/null 2>&1 || true
        pct destroy "${ctid}" --purge > /dev/null 2>&1 || true
    fi
    rm -rf "${work}"
}
trap cleanup EXIT

# fetch <url> <file> <sha256>: downloads and checks a file. Any failed transfer is tried again, a
# connection reset mid-way included, which --retry alone leaves out.
fetch() {
    curl -fsSL --proto '=https' --retry 5 --retry-all-errors --tlsv1.2 -o "$2" "$1" || fail "could not download $1"
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
        if [ -n "$2" ]; then
            env=/etc/kubehenge/appliance.env
            { grep -v "^KUBEHENGE_PROXMOX_INSECURE=" "$env" || true; [ "$2" = false ] || echo "KUBEHENGE_PROXMOX_INSECURE=true"; } > "$env.new"
            chmod 0644 "$env.new" && mv "$env.new" "$env"
        fi
        [ -f /etc/kubehenge/headlamp.env ] ||
            echo "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://$ip:8443/oidc-callback" > /etc/kubehenge/headlamp.env
        systemctl enable --now kubehenge-update.path >/dev/null 2>&1
        systemctl restart kubehenge.service
        # The next visit starts the new Headlamp.
        systemctl stop kubehenge-headlamp.service
    ' sh "${ctid}" "${insecure}" || rc=$?
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
fi
if [ -n "${proxmox_insecure}" ]; then
    api_ca=${cluster_ca}
    echo "kubehenge: the appliance and the clusters reach the Proxmox VE API at ${proxmox_url} without checking its certificate (--proxmox-insecure)" >&2
else
    choose_api_ca
fi

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
[ -z "${proxmox_insecure}" ] || echo 'KUBEHENGE_PROXMOX_INSECURE=true' >> "${work}/appliance.env"
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
started=1
sleep 2
pct exec "${ctid}" -- systemctl is-active --quiet kubehenge.service || fail "the service did not start; its log follows"
# The realms are what the interface asks Proxmox VE before anyone signs in: an answer shows that it
# reaches the API and trusts its certificate. The appliance's own certificate is checked as the one
# just installed: curl takes any certificate in --cacert as a trust anchor, the leaf included, so
# this holds for one Proxmox VE's CA signed, a self-signed one and the administrator's own alike.
for _ in $(seq 1 10); do
    realms=$(curl -s --cacert "${work}/tls.crt" --resolve "${public}:443:${ctip}" -o /dev/null -w '%{http_code}' "https://${public}/api/realms" || true)
    [ "${realms}" != 200 ] || break
    sleep 2
done
[ "${realms}" = 200 ] || fail "the appliance cannot use Proxmox VE's API at ${proxmox_url}; give a URL it reaches, whose certificate names it, with --proxmox-url (its log follows)"

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
