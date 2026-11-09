#!/bin/sh
# Starts a one-node Proxmox VE for the end-to-end tests on a Linux machine with Docker and /dev/kvm,
# such as a CI runner: Proxmox VE in a container (test/e2e/pve), a registry beside it for the images
# built from the tree under test, and a route from this machine to the VMs' bridge.
#
#   test/e2e/ci/up.sh <work directory>
#
# Writes <work directory>/env, the settings test/e2e/ci/run.sh reads. Needs sudo for the route.
# With KUBEHENGE_E2E_PVE_BUILT=1, the image kubehenge-e2e-pve already loaded is run as it is rather
# than built. With KUBEHENGE_E2E_IPV6=1, the Docker network and the route to the VMs' bridge carry
# IPv6 as well, which a cluster with IPv6 first needs to reach its API.
#
# The registry serves the images test/e2e/ci/build.sh left in <work directory>/build/registry.
#
#   172.21.99.0/24  the Docker network: this machine .1, Proxmox VE .2 (its API on 8006), registry .3
#   172.20.99.0/24  the VMs' bridge, NAT through the container, which is .1 and their name server
#                   (test/e2e/pve/dnsmasq.conf): management cluster .10 (API) and .11-.13,
#                   workload clusters .20-.29 and their load balancers .30-.39, the appliance
#                   .40, the VM that prepares a template .50
#   fd99:21:99::/64 the Docker network's IPv6 range (KUBEHENGE_E2E_IPV6=1): Proxmox VE ::2
#   fd99:20:99::/64 the VMs' bridge in IPv6, without NAT (downloads use IPv4): the container ::1
#                   and name server, workload clusters' API ::20, their nodes ::21-::29 and load
#                   balancers ::30-::39
set -eu

work=${1:?usage: $0 <work directory>}
script=$(readlink -f "$0")
here=$(dirname "${script}")
# renovate: datasource=docker depName=registry
REGISTRY_IMAGE=registry:3.1.2@sha256:d4032d4cc323c1731c707931ecada07a53076eac38e3b61ae57533f170f2c6c3

mkdir -p "${work}/vz"
work=$(readlink -f "${work}")
if [ "${KUBEHENGE_E2E_PVE_BUILT:-}" != 1 ]; then
    # A failed apt-get says why only in the build's own output, which -q leaves out.
    if ! docker build --progress=plain -t kubehenge-e2e-pve "${here}/../pve" > "${work}/pve-build.log" 2>&1; then
        tail -n 80 "${work}/pve-build.log" >&2
        exit 1
    fi
fi
if [ "${KUBEHENGE_E2E_IPV6:-}" = 1 ]; then
    docker network create --ipv6 --subnet 172.21.99.0/24 --gateway 172.21.99.1 \
        --subnet fd99:21:99::/64 kubehenge-e2e > /dev/null
    set -- --ip6 fd99:21:99::2
else
    set --
    docker network create --subnet 172.21.99.0/24 --gateway 172.21.99.1 kubehenge-e2e > /dev/null
fi
# Privileged for /dev/kvm, /dev/net/tun, /dev/fuse (Proxmox VE's configuration file system) and the
# bridge and NAT it sets up in its own network namespace. On a kernel with AppArmor, LXC loads a
# profile for each container through securityfs, which Docker does not mount, so it comes from the
# host as on a real node; without it no container starts. corosync, in a cluster of two, keeps the
# buffers of each client's connection in /dev/shm, whose 64 MiB Docker gives fill up: "No space left
# on device", and the cluster's services no longer answer. A tmpfs takes memory only as it fills.
docker run -d --name kubehenge-e2e-pve --hostname pve --privileged --cgroupns=private \
    --network kubehenge-e2e --ip 172.21.99.2 "$@" --tmpfs /run --tmpfs /run/lock --shm-size 1g \
    -v /sys/kernel/security:/sys/kernel/security -v "${work}/vz:/var/lib/vz" kubehenge-e2e-pve > /dev/null
# Its storage holds the images test/e2e/ci/build.sh built for the run, when it ran.
mkdir -p "${work}/build/registry"
docker run -d --name kubehenge-e2e-registry --network kubehenge-e2e --ip 172.21.99.3 \
    -v "${work}/build/registry:/var/lib/registry" -e OTEL_TRACES_EXPORTER=none "${REGISTRY_IMAGE}" > /dev/null

pve() { docker exec kubehenge-e2e-pve "$@"; }
deadline=$(($(date +%s) + 600))
# pvesh answers once the configuration file system is up; the API's certificate is written when
# pveproxy starts, after it.
# shellcheck disable=SC2310 # pve wraps one docker exec, whose status is what the loop polls and reports.
until pve systemctl is-active --quiet pveproxy.service && pve pvesh get /version > /dev/null 2>&1; do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "Proxmox VE did not start within 10 minutes" >&2
        pve systemctl --failed --no-pager >&2 || true
        pve journalctl -b --no-pager -n 200 >&2 || true
        exit 1
    fi
    sleep 3
done
# The firewall on nftables (proxmox-firewall), set before any VM starts, since a running guest moves
# to it only once it restarts. pve-firewall cannot load the legacy iptables tables on runner kernels
# and applies no rule. The nftables backend makes no fwbr bridges, so the NAT needs no conntrack zone.
# The node's own firewall stays off: the tests are of the VMs' firewall, and the node's would drop
# what the VMs send to its API and name server, since they are not on its local network.
pve pvesh set /nodes/pve/firewall/options --nftables 1 --enable 0
# Proxmox VE copies a volume through SSH to the node it goes to, this one too, so it must reach
# itself as it does then: as root, with its own key, checking only the host key `pvecm updatecerts`
# wrote under its name when it started.
known=/etc/pve/nodes/pve/ssh_known_hosts
# shellcheck disable=SC2310 # pve wraps one docker exec, whose status is what the loop polls and reports.
until ssh_error=$(pve ssh -e none -o UserKnownHostsFile="${known}" -o GlobalKnownHostsFile=none \
    -o BatchMode=yes -o HostKeyAlias=pve root@172.21.99.2 true 2>&1); do
    now=$(date +%s)
    if [ "${now}" -ge "${deadline}" ]; then
        echo "Proxmox VE does not reach itself over SSH: ${ssh_error}" >&2
        pve systemctl status --no-pager ssh-host-keys.service ssh.service >&2 || true
        exit 1
    fi
    sleep 3
done
# `import` holds the disk images VMs are created from, `rootdir` the appliance's container.
pve pvesm set local --content iso,vztmpl,backup,images,snippets,import,rootdir
# A storage no other cluster, the management cluster nor the templates use, for a cluster that
# holds Datastore.Allocate on its own.
pve pvesm add dir own --path /var/lib/vz/own --mkdir 1 --content images
pve cat /etc/pve/pve-root-ca.pem > "${work}/pve-root-ca.pem"
secret=$(pve pveum user token add root@pam e2e --privsep 0 --output-format json |
    sed -n 's/.*"value" *: *"\([^"]*\)".*/\1/p')
[ -n "${secret}" ]
sudo ip route replace 172.20.99.0/24 via 172.21.99.2
if [ "${KUBEHENGE_E2E_IPV6:-}" = 1 ]; then
    sudo ip -6 route replace fd99:20:99::/64 via fd99:21:99::2
fi

umask 077
cat > "${work}/env" << EOF
export KUBEHENGE_PROXMOX_URL=https://172.21.99.2:8006
export KUBEHENGE_PROXMOX_TOKEN='root@pam!e2e=${secret}'
export KUBEHENGE_PROXMOX_CA=${work}/pve-root-ca.pem
export KUBEHENGE_E2E_NODE=pve KUBEHENGE_E2E_STORAGE=local KUBEHENGE_E2E_BRIDGE=vmbr0
export KUBEHENGE_E2E_PREFIX=24 KUBEHENGE_E2E_GATEWAY=172.20.99.1 KUBEHENGE_E2E_DNS=172.20.99.1
export KUBEHENGE_E2E_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_ADDRESSES=172.20.99.21-172.20.99.29
export KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES=172.20.99.30-172.20.99.39
export KUBEHENGE_E2E_ADDRESS=172.20.99.50/24
export KUBEHENGE_E2E_IPV6_PREFIX=64 KUBEHENGE_E2E_IPV6_GATEWAY=fd99:20:99::1 KUBEHENGE_E2E_IPV6_DNS=fd99:20:99::1
export KUBEHENGE_E2E_IPV6_ENDPOINT=fd99:20:99::20 KUBEHENGE_E2E_IPV6_ADDRESSES=fd99:20:99::21-fd99:20:99::29
export KUBEHENGE_E2E_IPV6_LOAD_BALANCER_ADDRESSES=fd99:20:99::30-fd99:20:99::39
export KUBEHENGE_E2E_OIDC_ADDRESS=172.21.99.1
export KUBEHENGE_E2E_REGISTRY=172.21.99.3:5000
EOF
echo "Proxmox VE is up at https://172.21.99.2:8006; settings in ${work}/env"
