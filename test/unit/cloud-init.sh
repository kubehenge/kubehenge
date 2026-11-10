#!/bin/sh
# Unit test of the fixes the node images carry into cloud-init and of the dual-stack check after them
# (pkg/templates/node/cloud-init.sh): the module each fix changes, as it is before and after the fix
# (read from the fix itself), stand-ins for rpm and for cloud-init's net-convert, which renders the
# network only where the module is fixed, and the real patch.
#
#   test/unit/cloud-init.sh
set -eu

here=$(dirname "$0")
script=${here}/../../pkg/templates/node/cloud-init.sh
fix=${here}/../../pkg/templates/node/cloud-init/create-route-section-before-its-metric.patch
root=$(mktemp -d)
bin=${root}/bin site=${root}/site
module=${site}/cloudinit/net/network_manager.py
cache=${site}/cloudinit/net/__pycache__
mkdir -p "${bin}" "${cache}"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# truth <the wrong build it catches> <command>...: whether the command succeeds.
truth() {
    what=$1
    shift
    if "$@"; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n' "${what}"
        failures=$((failures + 1))
    fi
}
# said <the wrong build it catches> <text>: whether the last run printed the text.
said() {
    if grep -qF -- "$2" "${root}/out"; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted the build to say: %s\n  it said:\n' "$1" "$2"
        cat "${root}/out"
        failures=$((failures + 1))
    fi
}

# The module before and after the fix, from the fix's one hunk.
awk '/^@@/ { body = 1; next } body && /^[ -]/ { print substr($0, 2) }' "${fix}" > "${root}/before.py"
awk '/^@@/ { body = 1; next } body && /^[ +]/ { print substr($0, 2) }' "${fix}" > "${root}/after.py"

cat > "${bin}/rpm" << 'EOF'
#!/bin/sh
case "$*" in
    "-ql cloud-init") printf '/usr/bin/cloud-init\n%s/cloudinit/__init__.py\n%s/cloudinit/net/network_manager.py\n' "${SITE}" "${SITE}" ;;
    "-q cloud-init") echo cloud-init-24.4-7.el10 ;;
    *) exit 2 ;;
esac
EOF
# net-convert notes each network it is given and, where the module is the fixed one, writes the
# connection RENDER names: both metrics, the IPv6 one missing, or no connection at all. With the
# module unfixed, or for the network named in BREAK, it fails as cloud-init does.
cat > "${bin}/cloud-init" << 'EOF'
#!/bin/sh
[ "$1 $2" = "devel net-convert" ] || exit 2
shift 2
while [ "$#" -gt 0 ]; do
    case $1 in
        --network-data) config=$2 ;;
        --directory) out=$2 ;;
        --distro) distro=$2 ;;
    esac
    shift 2
done
echo "net-convert ${distro} $(basename "${config}")" >> "${CALLS}"
[ -z "${BREAK}" ] || [ "$(basename "${config}" .yaml)" != "${BREAK}" ] || exit 1
cmp -s "${SITE}/cloudinit/net/network_manager.py" "${FIXED}" || {
    echo "KeyError: 'ipv6'" >&2
    exit 1
}
[ "${RENDER}" != none ] || exit 0
mkdir -p "${out}/etc/NetworkManager/system-connections"
{
    printf '[connection]\nid=cloud-init eth0\n\n[ipv4]\nmethod=manual\nroute-metric=100\n\n[ipv6]\nmethod=manual\n'
    [ "${RENDER}" = ipv4-only ] || printf 'route-metric=100\n'
} > "${out}/etc/NetworkManager/system-connections/cloud-init-eth0.nmconnection"
EOF
chmod +x "${bin}/rpm" "${bin}/cloud-init"

# build <module> [<RENDER>] [<BREAK>]: a build of an image whose cloud-init has the module.
build() {
    cp "$1" "${module}"
    : > "${cache}/network_manager.cpython-312.pyc"
    : > "${cache}/network_manager.cpython-312.opt-1.pyc"
    : > "${cache}/network_state.cpython-312.pyc"
    : > "${root}/calls"
    code=0
    SITE=${site} FIXED=${root}/after.py RENDER=${2:-both} BREAK=${3:-} CALLS=${root}/calls PATH="${bin}:${PATH}" \
        sh "${script}" rhel > "${root}/out" 2>&1 || code=$?
}

build "${root}/before.py"
truth "a build that cannot carry the fix into the cloud-init it was written for fails" [ "${code}" = 0 ]
truth "a build that leaves cloud-init's renderer unfixed" cmp -s "${module}" "${root}/after.py"
said "a build that does not say what it carried" "carried create-route-section-before-its-metric into cloud-init-24.4-7.el10"
truth "a build that leaves the package's bytecode of the carried module" [ ! -e "${cache}/network_manager.cpython-312.pyc" ]
truth "or its optimised bytecode" [ ! -e "${cache}/network_manager.cpython-312.opt-1.pyc" ]
truth "a build that deletes the bytecode of a module it did not change" [ -e "${cache}/network_state.cpython-312.pyc" ]
truth "a build that checks only the IPv6-first network" grep -qx 'net-convert rhel dual-stack-ipv4-first.yaml' "${root}/calls"
truth "a build that checks only the IPv4-first network" grep -qx 'net-convert rhel dual-stack-ipv6-first.yaml' "${root}/calls"
said "a build that does not say the check passed" "renders dual-stack networks in both orders"

build "${root}/after.py"
truth "a build that fails on a cloud-init that has the fix already" [ "${code}" = 0 ]
truth "a build that changes a cloud-init that has the fix already" cmp -s "${module}" "${root}/after.py"
said "a build that does not say the fix was in cloud-init already" "cloud-init-24.4-7.el10 has create-route-section-before-its-metric already, so it is not carried"
truth "a build that deletes bytecode it did not make stale" [ -e "${cache}/network_manager.cpython-312.pyc" ]
truth "a build that skips the check where it carried nothing" grep -qx 'net-convert rhel dual-stack-ipv6-first.yaml' "${root}/calls"

sed 's/route_key = self._get_next_numbered_section/route_key = self._next_section/' "${root}/before.py" > "${root}/moved.py"
build "${root}/moved.py"
truth "a build that passes when the fix neither applies nor is in cloud-init" [ "${code}" != 0 ]
truth "a build that changes a module the fix no longer fits" cmp -s "${module}" "${root}/moved.py"
said "a build that does not name the fix it could not carry" "create-route-section-before-its-metric neither applies to cloud-init-24.4-7.el10 nor is in it"
truth "a build that checks the network after a fix failed to carry" [ ! -s "${root}/calls" ]

sed '1s/value = /value = str(/' "${root}/before.py" > "${root}/edge.py"
build "${root}/edge.py"
truth "a build that places a fix by fuzz where the lines around it changed" cmp -s "${module}" "${root}/edge.py"
truth "and passes" [ "${code}" != 0 ]

build "${root}/before.py" both dual-stack-ipv6-first
truth "a build that passes though the IPv6-first network fails to render" [ "${code}" != 0 ]
said "a build that does not name the network that failed" "cannot render the network of a dual-stack-ipv6-first node"

build "${root}/before.py" ipv4-only
truth "a build that passes with no metric under [ipv6]" [ "${code}" != 0 ]
said "a build that does not name the family without its metric" "without route-metric=100 under [ipv6] in cloud-init-eth0.nmconnection"

build "${root}/before.py" none
truth "a build that passes though cloud-init wrote no connection" [ "${code}" != 0 ]
said "a build that does not say no connection was rendered" "rendered no NetworkManager connection for the dual-stack-ipv4-first network"

echo "cloud-init: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
