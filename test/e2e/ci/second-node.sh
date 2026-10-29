#!/bin/sh
# Joins a second Proxmox VE node to the one test/e2e/ci/up.sh started, for the tests that need two:
#
#   test/e2e/ci/second-node.sh <work directory>
#
# The node, pve2, runs from the same image in a container of its own at 172.21.99.4 and joins pve in
# a cluster. Its VMs' bridge is joined to pve's at layer 2 through a Docker network of its own, whose
# ports are added to both bridges, so that VMs on either node share 172.20.99.0/24 with pve its
# gateway and name server as before. A directory both containers mount at /var/lib/vz/shared is the
# cluster's storage `shared`, as shared storage is to Proxmox VE: a template there is cloned onto
# either node. Adds the second node's settings to <work directory>/env.
set -eu

work=$(readlink -f "${1:?usage: $0 <work directory>}")
# shellcheck source=/dev/null
. "${work}/env"
: "${KUBEHENGE_E2E_ROOT_PASSWORD:?}"

on() {
    node=$1
    shift
    docker exec "kubehenge-e2e-${node}" "$@"
}

# The shared directory is in pve's own /var/lib/vz, which pve2 mounts again under the same path.
mkdir -p "${work}/vz/shared" "${work}/vz2"
docker run -d --name kubehenge-e2e-pve2 --hostname pve2 --privileged --cgroupns=private \
    --network kubehenge-e2e --ip 172.21.99.4 --tmpfs /run --tmpfs /run/lock \
    -v /sys/kernel/security:/sys/kernel/security -v "${work}/vz2:/var/lib/vz" \
    -v "${work}/vz/shared:/var/lib/vz/shared" kubehenge-e2e-pve > /dev/null

deadline=$(($(date +%s) + 600))
# shellcheck disable=SC2310 # on wraps one docker exec, whose status is what the loop polls and reports.
until on pve2 systemctl is-active --quiet pveproxy.service && on pve2 pvesh get /version > /dev/null 2>&1; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "Proxmox VE did not start on pve2 within 10 minutes" >&2
        on pve2 systemctl --failed --no-pager >&2 || true
        exit 1
    fi
    sleep 3
done

# Proxmox VE names its nodes by host name, which Docker's /etc/hosts knows only for the container
# itself.
on pve sh -c 'echo "172.21.99.4 pve2" >> /etc/hosts'
on pve2 sh -c 'echo "172.21.99.2 pve" >> /etc/hosts'

# One layer-2 segment for the VMs: a Docker network whose port in each container joins its bridge.
# pve2's bridge gives up the gateway's address, NAT and name server, which stay pve's, and the
# fixed address of pve's bridge, which on one segment would take pve's frames for its own.
docker network create --subnet 172.22.99.0/24 kubehenge-e2e-vms > /dev/null
for node in pve pve2; do
    docker network connect kubehenge-e2e-vms "kubehenge-e2e-${node}"
    port=$(on "${node}" ip -o -4 addr show | awk '$4 ~ /^172\.22\.99\./ { print $2; exit }')
    [ -n "${port}" ] || {
        echo "no port of the VMs' segment on ${node}" >&2
        exit 1
    }
    on "${node}" sh -c "ip addr flush dev ${port} && ip link set ${port} master vmbr0 up"
done
on pve2 sh -c 'ip addr del 172.20.99.1/24 dev vmbr0 && ip link set vmbr0 address 02:00:ac:14:63:02 &&
    systemctl stop dnsmasq.service &&
    iptables -t nat -D POSTROUTING -s 172.20.99.0/24 -o eth0 -j MASQUERADE'

on pve pvecm create kubehenge-e2e --link0 172.21.99.2 > /dev/null
fingerprint=$(on pve openssl x509 -in /etc/pve/nodes/pve/pve-ssl.pem -noout -fingerprint -sha256 | cut -d= -f2)
on pve2 pvesh create /cluster/config/join --hostname 172.21.99.2 --fingerprint "${fingerprint}" \
    --password "${KUBEHENGE_E2E_ROOT_PASSWORD}" --link0 172.21.99.4 > /dev/null
# Both nodes in the cluster's view, quorate, and pve2's API on its new certificate.
# shellcheck disable=SC2310 # polled; each check that fails is a cluster not there yet.
both_online() {
    on pve pvesh get /nodes --output-format json 2> /dev/null |
        jq -e '[.[] | select(.status == "online") | .node] | sort == ["pve", "pve2"]' > /dev/null &&
        on pve pvecm status 2> /dev/null | grep -q '^Quorate: *Yes' &&
        on pve2 pvesh get /version > /dev/null 2>&1
}
deadline=$(($(date +%s) + 600))
# shellcheck disable=SC2310 # both_online is what the loop polls and reports.
until both_online; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "pve2 did not join pve's cluster within 10 minutes" >&2
        on pve pvecm status >&2 || true
        on pve2 journalctl -u pve-cluster -u corosync --no-pager -n 100 >&2 || true
        exit 1
    fi
    sleep 3
done
on pve pvesm add dir shared --path /var/lib/vz/shared --shared 1 --content images,import,iso,snippets

cat >> "${work}/env" << EOF
export KUBEHENGE_E2E_NODES=pve,pve2 KUBEHENGE_E2E_SHARED_STORAGE=shared
EOF
echo "pve2 joined pve's cluster; storage shared on both"
