#!/bin/sh
# Gives the kubelet the node's IPs: the source address of the default route of each of the
# cluster's address families, in their order (/etc/kubehenge/ip-families, IPv4 alone when the file
# is missing). With an external cloud provider the kubelet sets no node addresses of its own. The
# Proxmox VE cloud controller manager takes them from the IPs the kubelet was given, and without
# them the API server reaches no kubelet. The first is the node's primary address, which has to be
# of the family of the Services' primary range. The documentation addresses 192.0.2.1 and
# 2001:db8::1 only pick the route: nothing is sent to them.
#
# A BGP router id is a dotted 32-bit id, so kube-vip on a node whose primary address is IPv6 takes
# the node's IPv4 address as its own. The ClusterClass leaves a placeholder for it in the manifest.
#   node-ip.sh kubeadm|rke2|k3s
set -eu

families=IPv4
if [ -r /etc/kubehenge/ip-families ]; then
    families=$(cat /etc/kubehenge/ip-families)
fi

ips=
ipv4=
for family in ${families}; do
    case "${family}" in
        IPv4)
            ip=$(ip -4 -o route get 192.0.2.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
            ipv4=${ip}
            ;;
        IPv6) ip=$(ip -6 -o route get 2001:db8::1 | sed -n 's/.* src \([0-9a-fA-F:]*\).*/\1/p') ;;
        *)
            echo "node-ip.sh: unknown address family ${family}" >&2
            exit 1
            ;;
    esac
    if [ -z "${ip}" ]; then
        echo "node-ip.sh: no ${family} route to pick the node's address from" >&2
        exit 1
    fi
    ips=${ips:+${ips},}${ip}
done

case "${1:-}" in
    kubeadm)
        printf 'KUBELET_EXTRA_ARGS=--node-ip=%s\n' "${ips}" > /etc/sysconfig/kubelet
        kube_vip=/etc/kubernetes/manifests/kube-vip.yaml
        ;;
    rke2 | k3s)
        mkdir -p "/etc/rancher/$1/config.yaml.d"
        printf 'node-ip: %s\n' "${ips}" > "/etc/rancher/$1/config.yaml.d/30-kubehenge-node-ip.yaml"
        kube_vip=/var/lib/rancher/$1/agent/pod-manifests/kube-vip.yaml
        ;;
    *)
        echo "usage: node-ip.sh kubeadm|rke2|k3s" >&2
        exit 2
        ;;
esac

if [ -f "${kube_vip}" ] && grep -q kubehenge-node-ipv4 "${kube_vip}"; then
    if [ -z "${ipv4}" ]; then
        echo "node-ip.sh: kube-vip needs a BGP router id, and the node has no IPv4 address to give it" >&2
        exit 1
    fi
    sed -i "s/kubehenge-node-ipv4/${ipv4}/" "${kube_vip}"
fi
