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
# either node. pve2 has memory of its own, as a second host would: its container's limit is the
# total it reports, and what its container uses, guests included, is its use. pve reports the
# runner's memory, pve2's use counted. Adds the second node's settings to <work directory>/env.
set -eu

work=$(readlink -f "${1:?usage: $0 <work directory>}")
# shellcheck source=/dev/null
. "${work}/env"
: "${KUBEHENGE_PROXMOX_URL:?}" "${KUBEHENGE_PROXMOX_TOKEN:?}" "${KUBEHENGE_PROXMOX_CA:?}"

on() {
    node=$1
    shift
    docker exec "kubehenge-e2e-${node}" "$@"
}

# pve2's memory in MiB: the most guests two-nodes.sh puts on it (a 4096 MiB control plane, a 1024 MiB
# worker and QEMU's 55 MiB each) and 1426 for its own services and a clone's page cache. Without
# guests, it has about 1.4 GiB more free than the 4505 MiB a node needs to rejoin a placement for
# that control plane.
pve2_mib=6656

# Proxmox VE reads a node's memory from /proc/meminfo, which in a container is the runner's. lxcfs,
# from the same image and run on the runner, serves a container a /proc/meminfo of its cgroup: its
# limit as the total and the memory its processes use. Its mount reaches the runner through the
# shared bind of its directory.
mkdir -p "${work}/lxcfs"
docker run -d --name kubehenge-e2e-lxcfs --privileged --pid=host --cgroupns=host \
    --mount "type=bind,source=${work}/lxcfs,target=/var/lib/lxcfs,bind-propagation=rshared" \
    kubehenge-e2e-pve lxcfs -f /var/lib/lxcfs > /dev/null
deadline=$(($(date +%s) + 30))
until [ -r "${work}/lxcfs/proc/meminfo" ]; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "lxcfs did not mount within 30 seconds" >&2
        docker logs kubehenge-e2e-lxcfs >&2 || true
        exit 1
    fi
    sleep 1
done

# The shared directory is in pve's own /var/lib/vz, which pve2 mounts again under the same path.
mkdir -p "${work}/vz/shared" "${work}/vz2"
# /dev/shm as large as up.sh gives pve's, for corosync.
docker run -d --name kubehenge-e2e-pve2 --hostname pve2 --privileged --cgroupns=private \
    --network kubehenge-e2e --ip 172.21.99.4 --tmpfs /run --tmpfs /run/lock --shm-size 1g \
    --memory "${pve2_mib}m" -v "${work}/lxcfs/proc/meminfo:/proc/meminfo" \
    -v /sys/kernel/security:/sys/kernel/security -v "${work}/vz2:/var/lib/vz" \
    -v "${work}/vz/shared:/var/lib/vz/shared" kubehenge-e2e-pve > /dev/null
total=$(on pve2 cat /proc/meminfo | awk '/^MemTotal:/ { print int($2 / 1024) }')
[ "${total}" = "${pve2_mib}" ] || {
    echo "pve2 reports ${total} MiB of memory, not the ${pve2_mib} MiB of its own" >&2
    exit 1
}

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
    iptables-nft -t nat -D POSTROUTING -s 172.20.99.0/24 -o eth0 -j MASQUERADE'

# pvecm reports a failed creation task and still exits 0.
on pve pvecm create kubehenge-e2e --link0 172.21.99.2
# shellcheck disable=SC2310 # the file missing is the failure reported.
on pve test -f /etc/pve/corosync.conf || {
    echo "pve made no cluster" >&2
    # Creating a cluster reloads sshd, the step that has failed in the container before.
    on pve systemctl status --no-pager ssh.service ssh.socket >&2 || true
    on pve journalctl -u ssh.service -u ssh.socket --no-pager -n 50 >&2 || true
    on pve sshd -t >&2 || true
    exit 1
}
# Creating the cluster restarts corosync and the cluster file system, which take seconds to be
# quorate again, and pve refuses a join until they are: "cluster not ready - no quorum?".
# shellcheck disable=SC2310 # polled; each check that fails is a cluster not quorate yet.
quorate() {
    on pve pvesh get /cluster/status --output-format json 2> /dev/null |
        jq -e 'any(.[]; .type == "cluster" and .quorate == 1)' > /dev/null
}
deadline=$(($(date +%s) + 120))
# shellcheck disable=SC2310 # quorate is what the loop polls and reports.
until quorate; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "pve's cluster was not quorate within 2 minutes of its creation" >&2
        on pve journalctl -u pve-cluster -u corosync --no-pager -n 100 >&2 || true
        exit 1
    fi
    sleep 2
done
fingerprint=$(on pve openssl x509 -in /etc/pve/nodes/pve/pve-ssl.pem -noout -fingerprint -sha256 | cut -d= -f2)
# Proxmox VE takes a join only with root@pam's password. It is random, set on pve for this join and
# kept out of env.
root_password=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
printf 'root:%s\n' "${root_password}" | docker exec -i kubehenge-e2e-pve chpasswd
# The join prints why it aborted, and exits 0 all the same.
on pve2 pvesh create /cluster/config/join --hostname 172.21.99.2 --fingerprint "${fingerprint}" \
    --password "${root_password}" --link0 172.21.99.4
# shellcheck disable=SC2310 # the file missing is the failure reported.
on pve2 test -f /etc/corosync/corosync.conf || {
    echo "pve2 did not join pve's cluster" >&2
    on pve journalctl -u pve-cluster -u corosync -u pveproxy -u pvedaemon --no-pager -n 100 >&2 || true
    exit 1
}
# The join's last step writes pve2's node files (its certificate, its SSH keys) once pve2 is quorate,
# not once its cluster file system has synced with pve's. Adding pve2 reloads corosync, and a write
# pve receives while that leaves it without quorum makes its file system leave the cluster's group
# and rejoin. Where pve2 comes up before pve is back, pve2 writes into a group of its own, and pve's
# rejoin then replaces those files with pve's, which lack them. So the step runs again once pve2
# holds pve's files.
deadline=$(($(date +%s) + 120))
# shellcheck disable=SC2310 # polled; pve's certificate missing is pve2 not synced with pve yet.
until on pve2 test -f /etc/pve/nodes/pve/pve-ssl.pem; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "pve2's cluster file system did not sync with pve's within 2 minutes" >&2
        on pve2 journalctl -u pve-cluster --no-pager -n 100 >&2 || true
        exit 1
    fi
    sleep 2
done
# shellcheck disable=SC2310 # a file missing is the step to run again.
if ! on pve test -f /etc/pve/nodes/pve2/pve-ssl.pem || ! on pve test -f /etc/pve/nodes/pve2/ssh_known_hosts; then
    echo "the join's node files for pve2 were lost to a resync of pve's cluster file system; writing them again"
    on pve2 pvecm updatecerts --force
    on pve2 systemctl reload-or-restart pvedaemon pveproxy
fi
# pve2's status as every API client reaches it: through pve's proxy, which accepts only the
# certificate the cluster's files hold for pve2. The join writes pve2's new one there and returns
# while pve2's proxy may still serve the old one, and pve then answers 596.
through_pve() {
    curl -sS --fail --max-time 20 --cacert "${KUBEHENGE_PROXMOX_CA}" \
        -H "Authorization: PVEAPIToken=${KUBEHENGE_PROXMOX_TOKEN}" \
        "${KUBEHENGE_PROXMOX_URL}/api2/json/nodes/pve2/status" > /dev/null
}
# Both nodes in the cluster's view, quorate, and pve2's API reached through pve.
# shellcheck disable=SC2310 # polled; each check that fails is a cluster not there yet.
both_online() {
    on pve pvesh get /nodes --output-format json 2> /dev/null |
        jq -e '[.[] | select(.status == "online") | .node] | sort == ["pve", "pve2"]' > /dev/null &&
        on pve pvecm status 2> /dev/null | grep -q '^Quorate: *Yes' &&
        through_pve 2> /dev/null
}
deadline=$(($(date +%s) + 600))
# shellcheck disable=SC2310 # both_online is what the loop polls and reports.
until both_online; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "pve2 did not join pve's cluster within 10 minutes" >&2
        on pve pvecm status >&2 || true
        through_pve >&2 || true
        on pve journalctl -u pve-cluster -u corosync -u pveproxy --no-pager -n 100 >&2 || true
        on pve2 journalctl -u pve-cluster -u corosync -u pveproxy --no-pager -n 100 >&2 || true
        exit 1
    fi
    sleep 3
done
# Each node chooses its firewall in its own host.fw, as up.sh does for pve.
on pve2 pvesh set /nodes/pve2/firewall/options --nftables 1
on pve pvesm add dir shared --path /var/lib/vz/shared --shared 1 --content images,import,iso,snippets

cat >> "${work}/env" << EOF
export KUBEHENGE_E2E_NODES=pve,pve2 KUBEHENGE_E2E_SHARED_STORAGE=shared
EOF
echo "pve2 joined pve's cluster; storage shared on both; pve2 has ${pve2_mib} MiB of memory"
