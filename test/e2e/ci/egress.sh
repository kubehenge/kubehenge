#!/bin/sh
# Puts the guests of an end-to-end lane behind TLS inspection or a proxy, and judges what the lane
# did behind it. Only the guests' bridge (172.20.99.0/24) is affected: the runner, Proxmox VE's own
# downloads and every connection to a private address go as they always do.
#
#   test/e2e/ci/egress.sh start <work directory> <kind>
#   test/e2e/ci/egress.sh stop <work directory>
#   test/e2e/ci/egress.sh check <work directory> <kind> <source>...
#
# <kind> is one of:
#   tls-inspection           mitmproxy in transparent mode re-signs every guest connection to port 443
#   tls-inspection-recovery  the same, for a lane that gives the Site CA only after a first failure
#   proxy                    only a CONNECT proxy that asks for credentials and tunnels leads out
#   proxy-inspecting         only a CONNECT proxy that re-signs TLS, with no credentials, leads out
#
# A workload cluster's proxy setting takes no credentials, since node files are plain text, so behind
# proxy its nodes go through a second listener of the same tunnelling proxy that asks for none.
# Behind proxy-inspecting the workload cluster sets no proxy and its nodes take the management side's.
#
# start writes <work directory>/egress/env for run.sh to read, and mitmproxy's log, flows.log, there.
# <source> is an address of the guests' bridge as an extended regular expression, such as
# 172\.20\.99\.1[1-3]: check fails unless the flow log shows each of them going through the proxy.
# The assertions run alone on a flow log:
#
#   test/e2e/ci/egress.sh intercepted <flow log> <source>...   # each made an HTTPS request mitmproxy re-signed
#   test/e2e/ci/egress.sh tunnelled <flow log> <source>...     # each opened a connection through the proxy
#   test/e2e/ci/egress.sh internal <flow log>                  # none asked for an internal address
#   test/e2e/ci/egress.sh leaks <secret file> <file>...        # none of the files holds the secret
#   test/e2e/ci/egress.sh ready <host> <port> <seconds>        # something accepts connections on the port
#
# The tls-inspection kinds re-sign with an intermediate whose issuer field names its root in other
# bytes than the root's own subject (test/e2e/tlsca), and the guests get only the root as the Site CA.
# Where only the proxy leads out, the guests' time server is the runner (test/e2e/ntp).
#
# mitmproxy's log at flow detail 1 holds a line "<client>:<port>: <method> <URL> <version>" for each
# request it read inside a connection it re-signed, and "[<time>][<client>:<port>] server connect
# <host>:<port> (<address>:<port>)" for each connection it opened, a tunnelled one included.
set -eu

guest_net=172.20.99.0/24
# What the guests reach without leaving the machine: the runner (172.21.99.1), Proxmox VE and the
# registry beside it, and the VMs' own bridge.
private_nets='10.0.0.0/8 172.16.0.0/12 192.168.0.0/16'
proxy_address=172.21.99.1
transparent_port=8080
proxy_port=3128
node_proxy_port=3129
proxy_user=e2e
repo=$(cd "$(dirname "$0")/../../.." && pwd)
# The exceptions the guests get: Proxmox VE, the networks of the lab and the management cluster's
# default pod and service ranges.
no_proxy='pve,localhost,127.0.0.1,.svc,.cluster.local,172.20.99.0/24,172.21.99.0/24,10.244.0.0/16,10.96.0.0/12'
table=kubehenge_e2e_egress
pve=kubehenge-e2e-pve

usage="usage: $0 start <work directory> <kind> | stop <work directory> | check <work directory> <kind> <source>..."

# kind_known <kind>: refuses a kind that is none of the four.
kind_known() {
    case $1 in
        tls-inspection | tls-inspection-recovery | proxy | proxy-inspecting) ;;
        *)
            echo "unknown kind $1" >&2
            exit 2
            ;;
    esac
}

# intercepted <flow log> <source>...
intercepted() {
    intercepted_log=$1
    shift
    intercepted_missing=0
    for intercepted_source in "$@"; do
        if ! grep -Eq "^${intercepted_source}:[0-9]+: [A-Z]+ https://" "${intercepted_log}"; then
            echo "${intercepted_source} made no HTTPS request that went through the intercepting proxy" >&2
            intercepted_missing=1
        fi
    done
    return "${intercepted_missing}"
}

# tunnelled <flow log> <source>...
tunnelled() {
    tunnelled_log=$1
    shift
    tunnelled_missing=0
    for tunnelled_source in "$@"; do
        if ! grep -Eq "^\[[0-9:.]+\]\[${tunnelled_source}:[0-9]+\] server connect " "${tunnelled_log}"; then
            echo "${tunnelled_source} opened no connection through the proxy" >&2
            tunnelled_missing=1
        fi
    done
    return "${tunnelled_missing}"
}

# internal <flow log>: fails when the proxy opened a connection to Proxmox VE or to a private address.
internal() {
    internal_hits=$(sed -n 's/^\[[0-9:.]*\]\[[^]]*\] server connect \([^ ]*\) .*/\1/p' "$1" |
        grep -E '^(pve(:|$)|(10|127)\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.)' | sort -u || true)
    if [ -n "${internal_hits}" ]; then
        internal_list=$(printf '%s' "${internal_hits}" | tr '\n' ' ')
        echo "the proxy was asked for internal addresses, which the exceptions leave out: ${internal_list}" >&2
        return 1
    fi
}

# leaks <secret file> <file>...: files that do not exist are skipped.
leaks() {
    leaks_secret=$1
    shift
    [ -s "${leaks_secret}" ] || return 0
    leaks_found=0
    for leaks_file in "$@"; do
        [ -f "${leaks_file}" ] || continue
        if grep -qFf "${leaks_secret}" "${leaks_file}"; then
            echo "${leaks_file} holds the proxy's password" >&2
            leaks_found=1
        fi
    done
    return "${leaks_found}"
}

# guest_nat <-I|-D>: the guests' public traffic leaves Proxmox VE's container with its own address
# where interception or the proxy's firewall has to see it, and as before to private addresses.
# Rules are inserted in front of the container's masquerade, which stays for what is left, and the
# last one inserted is the first to match: the proxy's own address is the only private one that
# keeps the guest's address, as the proxy's log tells guests apart by it.
guest_nat() {
    guest_nat_match=
    case ${kind} in
        tls-inspection*) guest_nat_match='-p tcp --dport 443' ;;
        *) ;;
    esac
    # shellcheck disable=SC2086 # one option or argument per word
    docker exec "${pve}" iptables-nft -t nat "$1" POSTROUTING -s "${guest_net}" -o eth0 ${guest_nat_match} -j ACCEPT
    for guest_nat_net in ${private_nets}; do
        docker exec "${pve}" iptables-nft -t nat "$1" POSTROUTING -s "${guest_net}" -o eth0 -d "${guest_nat_net}" -j MASQUERADE
    done
    case ${kind} in
        proxy*)
            for guest_nat_port in "${proxy_port}" "${node_proxy_port}"; do
                docker exec "${pve}" iptables-nft -t nat "$1" POSTROUTING -s "${guest_net}" -o eth0 \
                    -d "${proxy_address}" -p tcp --dport "${guest_nat_port}" -j ACCEPT
            done
            ;;
        *) ;;
    esac
}

# nft_rules: the runner's rules for the kind, as nft reads them.
nft_rules() {
    nft_private=$(printf '%s' "${private_nets}" | sed 's/ /, /g')
    printf 'add table ip %s\n' "${table}"
    case ${kind} in
        tls-inspection*)
            printf 'add chain ip %s prerouting { type nat hook prerouting priority dstnat; }\n' "${table}"
            printf 'add rule ip %s prerouting ip saddr %s ip daddr != { %s } tcp dport 443 counter redirect to :%s\n' \
                "${table}" "${guest_net}" "${nft_private}" "${transparent_port}"
            ;;
        *)
            printf 'add chain ip %s forward { type filter hook forward priority -10; }\n' "${table}"
            printf 'add rule ip %s forward ip saddr %s ip daddr != { %s } counter drop\n' \
                "${table}" "${guest_net}" "${nft_private}"
            ;;
    esac
}

# listening: whether mitmproxy listens and has its authority.
listening() {
    grep -q 'listening at' "${dir}/flows.log" && [ -s "${dir}/conf/mitmproxy-ca.pem" ]
}

# ready <host> <port> <seconds>: waits until a TCP connection to the port succeeds, which speaks
# nothing to what listens, and fails once the seconds have passed.
ready() {
    ready_waited=0
    # shellcheck disable=SC2016 # the inner shell expands its own arguments.
    until timeout 3 bash -c 'exec 3<> "/dev/tcp/$0/$1"' "$1" "$2" 2> /dev/null; do
        if [ "${ready_waited}" -ge "$3" ]; then
            echo "nothing accepted a connection on $1:$2 within $3 s" >&2
            return 1
        fi
        sleep 1
        ready_waited=$((ready_waited + 1))
    done
}

# time_server: answers NTP on the runner's side of the guests' bridge, and waits until a query
# there gets an answer a client takes.
time_server() {
    (cd "${repo}" && go build -o "${dir}/ntp" ./test/e2e/ntp)
    nohup sudo -n "${dir}/ntp" serve "${proxy_address}:123" > "${dir}/ntp.log" 2>&1 &
    echo "$!" > "${dir}/ntp.pid"
    time_waited=0
    until "${dir}/ntp" ask "${proxy_address}:123" 2> "${dir}/ntp.ask"; do
        if [ "${time_waited}" -ge 30 ]; then
            cat "${dir}/ntp.ask" "${dir}/ntp.log" >&2
            echo "the time server on ${proxy_address}:123 gave no usable answer in 30 s" >&2
            exit 1
        fi
        sleep 1
        time_waited=$((time_waited + 1))
    done
}

start() {
    command -v mitmdump > /dev/null || {
        echo "mitmdump is not installed: scripts/tools.sh installs it" >&2
        exit 1
    }
    command -v nft > /dev/null || sudo apt-get install -y -qq nftables > /dev/null
    umask 077
    # The earlier test of a lane keeps its log beside this one's.
    rm -rf "${dir}-previous"
    [ ! -d "${dir}" ] || mv "${dir}" "${dir}-previous"
    mkdir -p "${dir}"
    echo "${kind}" > "${dir}/kind"
    port=${proxy_port}
    case ${kind} in
        tls-inspection*)
            (cd "${repo}" && go run ./test/e2e/tlsca "${dir}/conf")
            port=${transparent_port}
            set -- --mode transparent --listen-host 0.0.0.0 --listen-port "${transparent_port}"
            ;;
        proxy)
            head -c 18 /dev/urandom | base64 | tr -d '/+=\n' > "${dir}/secret"
            password=$(cat "${dir}/secret")
            # Ignoring every host has mitmproxy pass each connection on untouched: a plain CONNECT proxy.
            set -- --listen-host 0.0.0.0 --listen-port "${proxy_port}" --ignore-hosts '.*' \
                --proxyauth "${proxy_user}:${password}"
            ;;
        *) set -- --listen-host 0.0.0.0 --listen-port "${proxy_port}" ;;
    esac
    # Appended to, as the nodes' listener behind proxy writes the same log.
    nohup mitmdump --set "confdir=${dir}/conf" --set flow_detail=1 --set termlog_verbosity=info "$@" \
        >> "${dir}/flows.log" 2>&1 &
    pid=$!
    echo "${pid}" > "${dir}/pid"
    waited=0
    # shellcheck disable=SC2310 # a failed probe is the wait going on
    until listening; do
        if [ "${waited}" -ge 60 ] || ! kill -0 "${pid}" 2> /dev/null; then
            cat "${dir}/flows.log" >&2
            echo "mitmproxy did not start listening in 60 s" >&2
            exit 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    # shellcheck disable=SC2310 # its failure is reported with mitmproxy's log
    if ! ready "${proxy_address}" "${port}" 30; then
        cat "${dir}/flows.log" >&2
        exit 1
    fi
    node_port=${proxy_port}
    if [ "${kind}" = proxy ]; then
        node_port=${node_proxy_port}
        nohup mitmdump --set "confdir=${dir}/conf" --set flow_detail=1 --set termlog_verbosity=info \
            --listen-host 0.0.0.0 --listen-port "${node_port}" --ignore-hosts '.*' >> "${dir}/flows.log" 2>&1 &
        echo "$!" > "${dir}/node-pid"
        # shellcheck disable=SC2310 # its failure is reported with mitmproxy's log
        if ! ready "${proxy_address}" "${node_port}" 30; then
            cat "${dir}/flows.log" >&2
            exit 1
        fi
    fi
    case ${kind} in
        tls-inspection*)
            cp "${dir}/conf/root.pem" "${dir}/ca.pem"
            cp "${dir}/conf/store-root.pem" "${dir}/store-ca.pem"
            ;;
        *)
            cp "${dir}/conf/mitmproxy-ca-cert.pem" "${dir}/ca.pem"
            time_server
            ;;
    esac
    guest_nat -I
    nft_rules | sudo nft -f -

    # What run.sh gives the settings of the management side: the authority and the proxy as a user
    # sets them. The template builds run on the runner and read them from the environment
    # (pkg/templates/proxy.go), so they get a file of their own.
    {
        echo "export KUBEHENGE_E2E_EGRESS_ENV='${dir}/template.env'"
        case ${kind} in
            tls-inspection*)
                echo "export KUBEHENGE_E2E_SITE_CA='${dir}/ca.pem'"
                echo "export KUBEHENGE_E2E_STORE_CA='${dir}/store-ca.pem'"
                # The appliance gives its template builds the root and the copy it derives for Go's
                # verifier, which podman's pulls in a build use.
                cat "${dir}/ca.pem" "${dir}/store-ca.pem" > "${dir}/template-ca.pem"
                echo "export KUBEHENGE_PROXY_CA_FILE='${dir}/template-ca.pem'" > "${dir}/template.env"
                ;;
            *)
                echo "export KUBEHENGE_E2E_PROXY_URL='http://${proxy_address}:${proxy_port}'"
                echo "export KUBEHENGE_E2E_NO_PROXY='${no_proxy}'"
                echo "export KUBEHENGE_E2E_TIME_SERVER='${proxy_address}'"
                if [ "${kind}" = proxy ]; then
                    echo "export KUBEHENGE_E2E_NODE_PROXY_URL='http://${proxy_address}:${node_port}'"
                    echo "export KUBEHENGE_E2E_PROXY_USER='${proxy_user}'"
                    echo "export KUBEHENGE_E2E_PROXY_PASSWORD_FILE='${dir}/secret'"
                    url="http://${proxy_user}:${password}@${proxy_address}:${proxy_port}"
                    : > "${dir}/template.env"
                else
                    echo "export KUBEHENGE_E2E_PROXY_CA='${dir}/ca.pem'"
                    url="http://${proxy_address}:${proxy_port}"
                    echo "export KUBEHENGE_PROXY_CA_FILE='${dir}/ca.pem'" > "${dir}/template.env"
                fi
                echo "export HTTPS_PROXY='${url}' HTTP_PROXY='${url}' NO_PROXY='${no_proxy}'" >> "${dir}/template.env"
                ;;
        esac
    } > "${dir}/env"
    echo "the guests are behind ${kind}, mitmproxy's log is ${dir}/flows.log"
}

stop() {
    [ -s "${dir}/pid" ] || return 0
    kind=$(cat "${dir}/kind")
    # The counters show how much the rules caught.
    sudo nft list table ip "${table}" 2> /dev/null || true
    sudo nft delete table ip "${table}" 2> /dev/null || true
    # shellcheck disable=SC2310 # a rule already gone is no error
    guest_nat -D 2> /dev/null || true
    pid=$(cat "${dir}/pid")
    kill "${pid}" 2> /dev/null || true
    rm -f "${dir}/pid"
    [ ! -s "${dir}/node-pid" ] || kill "$(cat "${dir}/node-pid")" 2> /dev/null || true
    rm -f "${dir}/node-pid"
    [ ! -s "${dir}/ntp.pid" ] || sudo -n kill "$(cat "${dir}/ntp.pid")" 2> /dev/null || true
    rm -f "${dir}/ntp.pid"
}

check() {
    case ${kind} in
        tls-inspection*) intercepted "${dir}/flows.log" "$@" ;;
        proxy)
            tunnelled "${dir}/flows.log" "$@"
            internal "${dir}/flows.log"
            ;;
        *)
            intercepted "${dir}/flows.log" "$@"
            internal "${dir}/flows.log"
            ;;
    esac
    # The test's own logs, which the workflow keeps.
    leaks "${dir}/secret" "${dir}/flows.log" "${work}"/*.log "${work}"/*/*.log "${RUNNER_TEMP:-/nonexistent}/test.log"
    echo "every source went through the ${kind}: $*"
}

mode=${1:-}
case ${mode} in
    intercepted | tunnelled | internal | leaks | ready)
        shift
        "${mode}" "$@"
        exit
        ;;
    start | stop | check) ;;
    *)
        echo "${usage}" >&2
        exit 2
        ;;
esac
work=$(readlink -f "${2:?${usage}}")
dir=${work}/egress
case ${mode} in
    start)
        kind=${3:?${usage}}
        kind_known "${kind}"
        shift 3
        start
        ;;
    stop) stop ;;
    *)
        kind=${3:?${usage}}
        kind_known "${kind}"
        shift 3
        check "$@"
        ;;
esac
