#!/bin/sh
# Starts a one-node Proxmox VE for the end-to-end tests on a Linux machine with Docker and /dev/kvm,
# such as a CI runner: Proxmox VE in a container (test/e2e/pve), a registry beside it for the images
# built from the tree under test, and a route from this machine to the VMs' bridge.
#
#   test/e2e/ci/up.sh <work directory>
#
# Writes <work directory>/env, the settings test/e2e/ci/run.sh reads. Needs sudo for the route.
# With KUBEHENGE_E2E_PVE_BUILT=1, the image kubehenge-e2e-pve already loaded is run as it is rather
# than built.
#
#   172.21.99.0/24  the Docker network: this machine .1, Proxmox VE .2 (its API on 8006), registry .3
#   172.20.99.0/24  the VMs' bridge, NAT through the container, which is .1 and their name server
#                   (test/e2e/pve/dnsmasq.conf): management cluster .10 (API) and .11-.13,
#                   workload clusters .20-.29 and their load balancers .30-.39, the appliance
#                   .40, the VM that prepares a template .50
set -eu

work=${1:?usage: $0 <work directory>}
script=$(readlink -f "$0")
here=$(dirname "${script}")
# renovate: datasource=docker depName=registry
REGISTRY_IMAGE=registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8

mkdir -p "${work}/vz"
work=$(readlink -f "${work}")
[ "${KUBEHENGE_E2E_PVE_BUILT:-}" = 1 ] || docker build -q -t kubehenge-e2e-pve "${here}/../pve" > /dev/null
docker network create --subnet 172.21.99.0/24 --gateway 172.21.99.1 kubehenge-e2e > /dev/null
# Privileged for /dev/kvm, /dev/net/tun, /dev/fuse (Proxmox VE's configuration file system) and the
# bridge and NAT it sets up in its own network namespace. On a kernel with AppArmor, LXC loads a
# profile for each container through securityfs, which Docker does not mount, so it comes from the
# host as on a real node; without it no container starts.
docker run -d --name kubehenge-e2e-pve --hostname pve --privileged --cgroupns=private \
    --network kubehenge-e2e --ip 172.21.99.2 --tmpfs /run --tmpfs /run/lock \
    -v /sys/kernel/security:/sys/kernel/security -v "${work}/vz:/var/lib/vz" kubehenge-e2e-pve > /dev/null
docker run -d --name kubehenge-e2e-registry --network kubehenge-e2e --ip 172.21.99.3 \
    -e OTEL_TRACES_EXPORTER=none "${REGISTRY_IMAGE}" > /dev/null

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
# `import` holds the disk images VMs are created from, `rootdir` the appliance's container.
pve pvesm set local --content iso,vztmpl,backup,images,snippets,import,rootdir
# A storage no other cluster, the management cluster nor the templates use, for a cluster that
# holds Datastore.Allocate on its own.
pve pvesm add dir own --path /var/lib/vz/own --mkdir 1 --content images
pve cat /etc/pve/pve-root-ca.pem > "${work}/pve-root-ca.pem"
secret=$(pve pveum user token add root@pam e2e --privsep 0 --output-format json |
    sed -n 's/.*"value" *: *"\([^"]*\)".*/\1/p')
[ -n "${secret}" ]
# root@pam's password, which the CSI plugin of a cluster taking volume snapshots signs in with: a
# fresh one, as the image comes with none.
root_password=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
printf 'root:%s\n' "${root_password}" | docker exec -i kubehenge-e2e-pve chpasswd
sudo ip route replace 172.20.99.0/24 via 172.21.99.2

umask 077
cat > "${work}/env" << EOF
export KUBEHENGE_PROXMOX_URL=https://172.21.99.2:8006
export KUBEHENGE_PROXMOX_TOKEN='root@pam!e2e=${secret}'
export KUBEHENGE_PROXMOX_CA=${work}/pve-root-ca.pem
export KUBEHENGE_E2E_ROOT_PASSWORD=${root_password}
export KUBEHENGE_E2E_NODE=pve KUBEHENGE_E2E_STORAGE=local KUBEHENGE_E2E_BRIDGE=vmbr0
export KUBEHENGE_E2E_PREFIX=24 KUBEHENGE_E2E_GATEWAY=172.20.99.1 KUBEHENGE_E2E_DNS=172.20.99.1
export KUBEHENGE_E2E_ENDPOINT=172.20.99.20 KUBEHENGE_E2E_ADDRESSES=172.20.99.21-172.20.99.29
export KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES=172.20.99.30-172.20.99.39
export KUBEHENGE_E2E_ADDRESS=172.20.99.50/24
export KUBEHENGE_E2E_OIDC_ADDRESS=172.21.99.1
export KUBEHENGE_E2E_REGISTRY=172.21.99.3:5000
EOF
echo "Proxmox VE is up at https://172.21.99.2:8006; settings in ${work}/env"
