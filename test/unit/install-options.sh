#!/bin/sh
# Unit test of what appliance/install.sh refuses before it downloads or creates anything: bad
# options, a missing bridge, an appliance that exists already, a cluster without quorum. The whole
# script runs against stand-ins for the Proxmox VE commands. Needs no Proxmox VE.
#
#   test/unit/install-options.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
bin=${root}/bin
mkdir "${bin}"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# run_installer <arguments>...: sets got to what the installer printed. It goes through a file, as a
# command substitution would start a process more.
run_installer() {
    sh "${installer}" "$@" > "${root}/said" 2>&1 || true
    got=
    while IFS= read -r line || [ -n "${line}" ]; do
        got=${got:+${got}
}${line}
    done < "${root}/said"
}
# says <what> <text wanted> <arguments>...: whether the installer, run with them, ends saying the text.
says() {
    what=$1 wanted=$2
    shift 2
    run_installer "$@"
    case "${got}" in
        *"${wanted}"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted it to say: %s\n  it said: %s\n' "${what}" "${wanted}" "${got}"
            failures=$((failures + 1))
            ;;
    esac
}
# does_not_say <what> <text unwanted> <arguments>...
does_not_say() {
    what=$1 unwanted=$2
    shift 2
    run_installer "$@"
    case "${got}" in
        *"${unwanted}"*)
            printf 'FAIL: %s\n  it must not say: %s\n  it said: %s\n' "${what}" "${unwanted}" "${got}"
            failures=$((failures + 1))
            ;;
        *) passes=$((passes + 1)) ;;
    esac
}

here_node=$(hostname)
printf '#!/bin/sh\necho 0\n' > "${bin}/id"
printf '#!/bin/sh\nexit 0\n' > "${bin}/pct"
printf '#!/bin/sh\nexit 0\n' > "${bin}/pveam"
printf '#!/bin/sh\necho pve-manager/9.0.3/abc\n' > "${bin}/pveversion"
# The stand-ins print their files with the shell's own read, as every process they start costs time
# on Windows.
# shellcheck disable=SC2016 # the stand-ins expand them.
show='show() {
    [ -r "$1" ] || return 1
    while IFS= read -r show_line || [ -n "${show_line}" ]; do printf "%s\n" "${show_line}"; done < "$1"
}'
cat > "${bin}/pvecm" << EOF
#!/bin/sh
${show}
show "${root}/quorum" 2> /dev/null || exit 1
EOF
# pvesh answers from files: resources, network, vnets, and taken, the ids guests have.
cat > "${bin}/pvesh" << EOF
#!/bin/sh
${show}
case "\$2" in
    /cluster/resources) show "${root}/resources" ;;
    /nodes/*/network) show "${root}/network" ;;
    /cluster/sdn/vnets) show "${root}/vnets" 2> /dev/null || exit 1 ;;
    /cluster/nextid)
        if [ "\${3:-}" = --vmid ] && grep -qx "\$4" "${root}/taken" 2> /dev/null; then
            echo "VM \$4 already exists" >&2
            exit 1
        fi
        echo 100
        ;;
    *) exit 1 ;;
esac
EOF
cat > "${bin}/ip" << 'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "${bin}"/*
PATH=${bin}:${PATH}

printf 'Quorate:          Yes\n' > "${root}/quorum"
printf '[]\n' > "${root}/resources"
printf '[{"iface":"vmbr0","type":"bridge"},{"iface":"eno1","type":"eth"},{"iface":"vmbr1","type":"OVSBridge"}]\n' > "${root}/network"
# --dns keeps the runs past the name server check whatever this machine's resolver names.
release='--release /dev/null --sha256 00 --dns 192.0.2.53'

# A missing value.
says "an option without its value" "--name needs a value" --name
says "the last option without its value" "--storage needs a value" --ctid 105 --storage
says "a flag option needs none" "unknown option --nope" --nope

# Addresses.
# shellcheck disable=SC2086 # $release is several words.
{
    says "an IPv6 address" "--ip is dhcp or an IPv4 address with its prefix length" --ip 2001:db8::5/64 ${release}
    says "an IPv4 address without a prefix" "--ip is dhcp or an IPv4 address with its prefix length" --ip 192.0.2.5 ${release}
    says "an IPv4 address with a prefix over 32" "--ip is dhcp or an IPv4 address" --ip 192.0.2.5/33 ${release}
    says "an octet over 255" "--ip is dhcp or an IPv4 address" --ip 192.0.2.256/24 ${release}
    says "an IPv6 gateway" "--gateway is an IPv4 address" --ip 192.0.2.5/24 --gateway 2001:db8::1 ${release}
    does_not_say "a good static address" "--ip is dhcp or" --ip 192.0.2.5/24 --gateway 192.0.2.1 ${release}
    says "a VLAN tag of 0" "--vlan is a VLAN tag from 1 to 4094" --vlan 0 ${release}
    says "a VLAN tag over 4094" "--vlan is a VLAN tag from 1 to 4094" --vlan 4095 ${release}
    says "a VLAN tag that is no number" "--vlan is a VLAN tag" --vlan x ${release}
    says "an MTU under 576" "--mtu is a number of bytes from 576 to 65535" --mtu 100 ${release}
    does_not_say "a good VLAN tag and MTU" "--vlan is" --vlan 4094 --mtu 9000 ${release}
    says "an OpenTofu archive that is not there" "--tofu names a readable OpenTofu release archive" --tofu "${root}/missing.tar.gz" ${release}
    says "a trusted proxy that is a name" "--trusted-proxy proxy.example: use an IPv4 or IPv6 address or CIDR range" --trusted-proxy proxy.example ${release}
    says "a bad one among good trusted proxies" "--trusted-proxy 10.0.0.0/33: use an IPv4" --trusted-proxy 192.0.2.7 --trusted-proxy 10.0.0.0/33 ${release}
    says "a bad one in a list" "--trusted-proxy 300.0.0.1: use an IPv4" --trusted-proxy 192.0.2.7,300.0.0.1 ${release}
    says "--trusted-proxy without its value" "--trusted-proxy needs a value" --trusted-proxy
    says "--trusted-proxy with --certificate" "--trusted-proxy goes with an install or --upgrade" --certificate --ctid 105 --trusted-proxy 192.0.2.7
    says "--no-trusted-proxy on an install" "--no-trusted-proxy goes with --upgrade" --no-trusted-proxy ${release}
    says "both trusted proxy options" "--trusted-proxy and --no-trusted-proxy exclude each other" --upgrade --ctid 105 --trusted-proxy 192.0.2.7 --no-trusted-proxy
    does_not_say "good trusted proxies" "--trusted-proxy " --trusted-proxy 192.0.2.7 --trusted-proxy 10.0.0.0/8,2001:db8::/32 ${release}
}

# Quorum.
printf 'Quorate:          No\n' > "${root}/quorum"
# shellcheck disable=SC2086 # $release is several words.
says "a cluster without quorum" "this node has no quorum in its Proxmox VE cluster" ${release}
printf 'Quorate:          Yes\n' > "${root}/quorum"

# The bridge, before anything is downloaded.
# shellcheck disable=SC2086 # $release is several words.
{
    says "a bridge that does not exist" "nope is no bridge, Open vSwitch bridge or SDN VNet of this node. Name one of vmbr0, vmbr1 with --bridge" --bridge nope ${release}
    printf '[{"vnet":"zone1v","zone":"zone1"}]\n' > "${root}/vnets"
    says "an SDN VNet is listed too" "Name one of vmbr0, vmbr1, zone1v with --bridge" --bridge nope ${release}
    does_not_say "an SDN VNet is a bridge" "is no bridge" --bridge zone1v ${release}
    does_not_say "an Open vSwitch bridge is a bridge" "is no bridge" --bridge vmbr1 ${release}
    rm "${root}/vnets"
    printf '[{"iface":"nic0","type":"eth"}]\n' > "${root}/network"
    says "no bridge to default to" "no bridge carries this node's default route and there is no vmbr0. Name one of none with --bridge" ${release}
    printf '[{"iface":"vmbr0","type":"bridge"}]\n' > "${root}/network"
}

# An id another guest has, which the cleanup after a failure would otherwise remove.
printf '107\n' > "${root}/taken"
# shellcheck disable=SC2086 # $release is several words.
{
    says "a --ctid another guest has" "--ctid 107 is taken by another guest. Name a free id, or leave --ctid out" --ctid 107 ${release}
    does_not_say "a free --ctid" "is taken" --ctid 108 ${release}
}
rm "${root}/taken"

# An appliance that exists already, anywhere in the cluster.
printf '[{"type":"lxc","vmid":105,"node":"pve2","tags":"kubehenge","name":"kubehenge"}]\n' > "${root}/resources"
# shellcheck disable=SC2086 # $release is several words.
says "an appliance on another node" "an appliance exists as container 105 on pve2. Run this with --upgrade on that node." ${release}
printf '[{"type":"lxc","vmid":105,"node":"pve2","tags":"web;kubehenge;x","name":"a"}]\n' > "${root}/resources"
# shellcheck disable=SC2086 # $release is several words.
says "a tag among others" "an appliance exists as container 105 on pve2" ${release}
printf '[{"type":"lxc","vmid":105,"node":"pve2","tags":"notkubehenge","name":"a"},{"type":"qemu","vmid":106,"node":"pve2","tags":"kubehenge","name":"b"}]\n' > "${root}/resources"
# shellcheck disable=SC2086 # $release is several words.
does_not_say "a tag that only contains the name, and a VM" "an appliance exists" ${release}

# --upgrade and --certificate on the wrong node.
printf '[{"type":"lxc","vmid":105,"node":"elsewhere-%s","tags":"kubehenge","name":"kubehenge"}]\n' "${here_node}" > "${root}/resources"
says "--upgrade on another node" "container 105 runs on node elsewhere-${here_node}. Run this there." --upgrade --ctid 105 --release /dev/null --sha256 00
says "--certificate on another node" "container 105 runs on node elsewhere-${here_node}. Run this there." --certificate --ctid 105
printf '[{"type":"lxc","vmid":105,"node":"pve2","tags":"notkubehenge","name":"a"}]\n' > "${root}/resources"
says "a container that is no appliance" "container 105 is not Kubehenge's appliance" --certificate --ctid 105

if [ "${failures}" -ne 0 ]; then
    echo "install-options: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-options: all ${passes} checks passed"
