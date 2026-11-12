#!/bin/sh
# Unit test of charts/kubehenge/files/node-ip.sh on kubeadm's nodes: a stand-in for ip answers with
# the node's address of each family, and a copy of the script writes the kubelet's arguments and
# kubeadm's configuration here instead of on the node.
#
# The stand-in is a shell function and the copy runs in a subshell that sources it, as each process
# started costs most of this test's time on Windows.
#
#   test/unit/node-ip.sh
set -eu

here=$(dirname "$0")
chart=${here}/../../charts/kubehenge
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0 got=
# expect <what> <wanted> <command>...: whether the command sets got to what is wanted.
expect() {
    what=$1 wanted=$2
    shift 2
    got=
    "$@" || true
    if [ "${got}" = "${wanted}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "${what}" "${wanted}" "${got}"
        failures=$((failures + 1))
    fi
}

# The placeholder the ClusterClass sets for init and for a control-plane join is the one the script
# replaces.
placeholders() {
    got=$(grep -c -F 'advertiseAddress: kubehenge-node-address' "${chart}/templates/kubeadm.yaml")
}
expect "the kubeadm ClusterClass names the placeholder for init and join" 2 placeholders

sed -e "s#/etc/kubehenge/ip-families#${root}/ip-families#g" \
    -e "s#/etc/sysconfig/kubelet#${root}/kubelet#g" \
    -e "s#/run/kubeadm/#${root}/#g" \
    -e "s#/etc/kubernetes/manifests/kube-vip.yaml#${root}/kube-vip.yaml#g" \
    "${chart}/files/node-ip.sh" > "${root}/node-ip.sh"
if grep -q -e ' /etc/' -e ' /run/' "${root}/node-ip.sh"; then
    echo "FAIL: the copy of the script still writes to the node's own paths:"
    grep -e ' /etc/' -e ' /run/' "${root}/node-ip.sh"
    exit 1
fi

# Not ipv4 and ipv6, which the sourced script sets for itself.
node_ipv4=10.0.0.12
node_ipv6=fd99:20:99::22
# The node's routes as iproute2 prints them, each family's only where the node has it.
ip() {
    case "$*" in
        "-4 -o route get 192.0.2.1")
            [ -z "${has_ipv4}" ] || printf '192.0.2.1 via 10.0.0.1 dev ens18 src %s uid 0 \\    cache \n' "${node_ipv4}"
            ;;
        "-6 -o route get 2001:db8::1")
            [ -z "${has_ipv6}" ] || printf '2001:db8::1 from :: via fd99:20:99::1 dev ens18 proto static src %s metric 100 pref medium\n' "${node_ipv6}"
            ;;
        *) return 2 ;;
    esac
}

# The configuration CABPK writes, as kubeadm's v1beta4: $1 is init or join, $2 the advertised address.
config() {
    case "$1" in
        init) printf 'apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nlocalAPIEndpoint:\n  advertiseAddress: %s\nnodeRegistration:\n  name: node\n' "$2" > "${root}/kubeadm.yaml" ;;
        join) printf 'apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\ncontrolPlane:\n  localAPIEndpoint:\n    advertiseAddress: %s\nnodeRegistration:\n  name: node\n' "$2" > "${root}/kubeadm-join-config.yaml" ;;
        worker) printf 'apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\nnodeRegistration:\n  name: node\n' > "${root}/kubeadm-join-config.yaml" ;;
        ipv4-first) printf 'apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  name: node\n' > "${root}/kubeadm.yaml" ;;
        *) return 2 ;;
    esac
}

# node <families, or "" for none named> <kubeadm configuration> [placeholder]: a node before the script.
node() {
    rm -f "${root}/ip-families" "${root}/kubelet" "${root}/kubeadm.yaml" "${root}/kubeadm-join-config.yaml"
    has_ipv4=''
    has_ipv6=''
    case "$1" in *IPv4*) has_ipv4=yes ;; "") has_ipv4=yes ;; *) ;; esac
    case "$1" in *IPv6*) has_ipv6=yes ;; *) ;; esac
    [ -z "$1" ] || echo "$1" > "${root}/ip-families"
    config "$2" "${3:-kubehenge-node-address}"
    cp "${root}/kubeadm.yaml" "${root}/before" 2> /dev/null || cp "${root}/kubeadm-join-config.yaml" "${root}/before"
}
run() {
    if (
        set -- kubeadm
        # shellcheck source=/dev/null
        . "${root}/node-ip.sh"
    ) > "${root}/said" 2>&1; then
        got=0
    else
        got=$?
    fi
}
# advertised <file>: sets got to the address the configuration advertises.
advertised() {
    got=$(sed -n 's/^ *advertiseAddress: //p' "${root}/$1")
}
kubelet() { read -r got < "${root}/kubelet"; }
unchanged() {
    if cmp -s "${root}/before" "${root}/$1"; then got=unchanged; else got=changed; fi
}

node "IPv6 IPv4" init
expect "IPv6 first, init: the script succeeds" 0 run
expect "IPv6 first, init: kubeadm advertises the node's IPv6 address" "\"${node_ipv6}\"" advertised kubeadm.yaml
expect "IPv6 first: the kubelet gets the IPv6 address first" "KUBELET_EXTRA_ARGS=--node-ip=${node_ipv6},${node_ipv4}" kubelet

node "IPv6 IPv4" join
expect "IPv6 first, control-plane join: the script succeeds" 0 run
expect "IPv6 first, control-plane join: kubeadm advertises the node's IPv6 address" "\"${node_ipv6}\"" advertised kubeadm-join-config.yaml

node "IPv6 IPv4" init '"kubehenge-node-address"'
run
expect "a quoted placeholder is replaced with its quotes" "\"${node_ipv6}\"" advertised kubeadm.yaml

node "IPv6" init
expect "IPv6 only: the script succeeds" 0 run
expect "IPv6 only: kubeadm advertises the node's IPv6 address" "\"${node_ipv6}\"" advertised kubeadm.yaml
expect "IPv6 only: the kubelet gets the IPv6 address alone" "KUBELET_EXTRA_ARGS=--node-ip=${node_ipv6}" kubelet

node "IPv6" join
run
expect "IPv6 only, control-plane join: kubeadm advertises the node's IPv6 address" "\"${node_ipv6}\"" advertised kubeadm-join-config.yaml

node "IPv4 IPv6" ipv4-first
expect "IPv4 first: the script succeeds" 0 run
expect "IPv4 first: kubeadm's configuration is left as it is" unchanged unchanged kubeadm.yaml
expect "IPv4 first: the kubelet gets the IPv4 address first" "KUBELET_EXTRA_ARGS=--node-ip=${node_ipv4},${node_ipv6}" kubelet

node "" ipv4-first
expect "IPv4 alone: the script succeeds" 0 run
expect "IPv4 alone: kubeadm's configuration is left as it is" unchanged unchanged kubeadm.yaml
expect "IPv4 alone: the kubelet gets the IPv4 address" "KUBELET_EXTRA_ARGS=--node-ip=${node_ipv4}" kubelet

node "IPv6 IPv4" worker
expect "a worker: the script succeeds" 0 run
expect "a worker's join configuration is left as it is" unchanged unchanged kubeadm-join-config.yaml

echo "node-ip: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
