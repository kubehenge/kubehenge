#!/bin/sh
# Carries the fixes in cloud-init/ into the image's cloud-init until its distribution ships them, then
# has cloud-init render the network capmox writes for a dual-stack node, IPv4 first and IPv6 first:
# one NIC with an address of each family and a default route of each, both with capmox's metric.
# NetworkManager is the renderer cloud-init picks on every family whose build runs this. The build
# stops when a fix neither applies nor is in cloud-init already, or when either network fails to
# render with the metric in both families.
#
#   cloud-init.sh <cloud-init's name for the distribution>
set -eu

dir=$(dirname "$0")/cloud-init
distro=$1

fail() {
    printf 'cloud-init.sh: %s\n' "$*" >&2
    exit 1
}

package=$(rpm -ql cloud-init | grep '/cloudinit/__init__\.py$') || fail "the cloud-init package holds no cloudinit module"
count=$(printf '%s\n' "${package}" | wc -l)
[ "${count}" -eq 1 ] || fail "the cloud-init package holds more than one cloudinit module: ${package}"
site=${package%/cloudinit/__init__.py}
version=$(rpm -q cloud-init)

for fix in "${dir}"/*.patch; do
    name=$(basename "${fix}" .patch)
    # No fuzz: a fix whose surroundings changed is looked at again, never placed by a guess.
    if patch -d "${site}" -p1 --batch --forward --fuzz=0 --dry-run < "${fix}" > /dev/null; then
        patch -d "${site}" -p1 --batch --forward --fuzz=0 --quiet < "${fix}"
        # The package's bytecode may be of a kind Python never checks against the source.
        sed -n 's|^+++ b/\(.*\)\.py$|\1|p' "${fix}" | while read -r module; do
            rm -f "${site}/$(dirname "${module}")/__pycache__/$(basename "${module}")".*.pyc
        done
        echo "cloud-init.sh: carried ${name} into ${version}"
    elif patch -d "${site}" -p1 --batch --reverse --forward --fuzz=0 --dry-run < "${fix}" > /dev/null; then
        echo "cloud-init.sh: ${version} has ${name} already, so it is not carried"
    else
        fail "${name} neither applies to ${version} nor is in it; check whether the distribution fixed it another way and drop or rebase it"
    fi
done

out=$(mktemp -d)
trap 'rm -rf "${out}"' EXIT
for config in "${dir}"/dual-stack-*.yaml; do
    order=$(basename "${config}" .yaml)
    cloud-init devel net-convert --network-data "${config}" --kind yaml --output-kind network-manager \
        --distro "${distro}" --directory "${out}/${order}" ||
        fail "${version} cannot render the network of a ${order} node (${config})"
    rendered=false
    for connection in "${out}/${order}"/etc/NetworkManager/system-connections/*.nmconnection; do
        [ -f "${connection}" ] || break
        rendered=true
        for family in ipv4 ipv6; do
            awk -v section="[${family}]" '
                $0 == section { inside = 1; next }
                /^\[/ { inside = 0 }
                inside && $0 == "route-metric=100" { found = 1 }
                END { exit !found }' "${connection}" ||
                fail "${version} rendered the ${order} network without route-metric=100 under [${family}] in $(basename "${connection}")"
        done
    done
    [ "${rendered}" = true ] || fail "${version} rendered no NetworkManager connection for the ${order} network"
done
echo "cloud-init.sh: ${version} renders dual-stack networks in both orders"
