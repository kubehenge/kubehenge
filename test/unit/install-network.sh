#!/bin/sh
# Unit test of the parts of appliance/install.sh that look at this node and its cluster: which
# bridge the container attaches to, the container's network setting, the appliances of the cluster,
# the node's resolver, the questions and the cleanup on a dropped session. Stand-ins answer for the
# Proxmox VE commands. Needs no Proxmox VE.
#
#   test/unit/install-network.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
bin=${root}/bin
mkdir "${bin}"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# expect <what> <wanted> <command>...: whether the command prints what is wanted.
expect() {
    what=$1 wanted=$2
    shift 2
    # A function that stops the script with fail exits its own subshell only.
    got=$( ("$@") 2>&1 || true)
    if [ "${got}" = "${wanted}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "${what}" "${wanted}" "${got}"
        failures=$((failures + 1))
    fi
}
# yes_or_no <command>...: yes when the command succeeds.
yes_or_no() {
    if "$@" > /dev/null 2>&1; then echo yes; else echo no; fi
}

cat > "${bin}/pvesh" << EOF
#!/bin/sh
case "\$2" in
    /cluster/resources) cat "${root}/resources" ;;
    /nodes/*/network) cat "${root}/network" ;;
    /cluster/sdn/vnets) cat "${root}/vnets" 2> /dev/null || exit 1 ;;
    *) exit 1 ;;
esac
EOF
cat > "${bin}/ip" << EOF
#!/bin/sh
cat "${root}/route" 2> /dev/null
EOF
cat > "${bin}/pvecm" << EOF
#!/bin/sh
cat "${root}/quorum" 2> /dev/null || exit 1
EOF
# pct exec <id> -- <command>: getent succeeds for the names in $root/resolvable. The route query
# prints $root/gateway. ping succeeds for the gateways in $root/pinging, and is missing (127) where
# $root/noping exists. A connection by bash succeeds for the host:port lines of $root/open.
cat > "${bin}/pct" << EOF
#!/bin/sh
case "\$1" in
    exec)
        case "\$4" in
            getent) grep -qxF -- "\$6" "${root}/resolvable" ;;
            timeout) grep -qxF -- "\${10}:\${11}" "${root}/open" ;;
            sh)
                case "\$6" in
                    *'route show'*) cat "${root}/gateway" 2> /dev/null ;;
                    *) [ ! -e "${root}/noping" ] || exit 127
                       grep -qxF -- "\$8" "${root}/pinging" ;;
                esac
                ;;
            *) exit 2 ;;
        esac
        ;;
    config) cat "${root}/config" 2> /dev/null ;;
    *) exit 2 ;;
esac
EOF
chmod +x "${bin}"/*
PATH=${bin}:${PATH}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"
node=pve1 tty=${root}/no-terminal

# Bridges.
printf '[{"iface":"vmbr0","type":"bridge"},{"iface":"bond0","type":"bond"},{"iface":"vmbr7","type":"OVSBridge"},{"type":"eth","iface":"eno1"}]\n' > "${root}/network"
printf '[{"vnet":"vn1","zone":"z"}]\n' > "${root}/vnets"
expect "bridges, Open vSwitch bridges and VNets, no bond" "vmbr0, vmbr7, vn1" bridge_list
expect "an existing bridge" yes yes_or_no bridge_exists vmbr7
expect "an SDN VNet" yes yes_or_no bridge_exists vn1
expect "a bond is none" no yes_or_no bridge_exists bond0
expect "a name only partly like one" no yes_or_no bridge_exists vmbr
rm "${root}/vnets"
expect "SDN absent: the node's bridges alone" "vmbr0, vmbr7" bridge_list

printf 'default via 192.0.2.1 dev vmbr7 proto kernel onlink\n' > "${root}/route"
expect "the bridge of the default route" vmbr7 default_bridge
printf 'default via 192.0.2.1 dev bond0.20 proto static\n' > "${root}/route"
expect "a default route on a VLAN interface: vmbr0" vmbr0 default_bridge
: > "${root}/route"
expect "no default route: vmbr0" vmbr0 default_bridge
printf '[{"iface":"vmbr9","type":"bridge"}]\n' > "${root}/network"
expect "neither a route's bridge nor vmbr0: none" "" default_bridge
expect "and it fails" no yes_or_no default_bridge

# The container's network.
bridge=vmbr0 vlan='' mtu='' ip=dhcp gateway=192.0.2.1
expect "DHCP: no gateway" "name=eth0,bridge=vmbr0,ip=dhcp" container_net
vlan=20 mtu=1400 ip=192.0.2.40/24
expect "a tag, an MTU and a static address" "name=eth0,bridge=vmbr0,tag=20,mtu=1400,ip=192.0.2.40/24,gw=192.0.2.1" container_net
gateway=''
vlan=''
expect "a static address without a gateway" "name=eth0,bridge=vmbr0,mtu=1400,ip=192.0.2.40/24" container_net

expect "an IPv4 address" yes yes_or_no is_ipv4 192.0.2.1
expect "an IPv6 address" no yes_or_no is_ipv4 2001:db8::1
expect "an octet over 255" no yes_or_no is_ipv4 192.0.2.300
expect "a name" no yes_or_no is_ipv4 pve.example.org
expect "an address with a prefix" yes yes_or_no is_ipv4_cidr 192.0.2.1/24
expect "an IPv6 prefix" no yes_or_no is_ipv4_cidr 2001:db8::1/64
expect "a prefix of 0" no yes_or_no is_ipv4_cidr 192.0.2.1/0

# The appliances of the cluster.
cat > "${root}/resources" << 'EOF'
[{"type":"qemu","vmid":100,"node":"pve1","tags":"kubehenge"},{"type":"lxc","vmid":101,"node":"pve1","tags":"notkubehenge"},{"cpu":0,"id":"lxc/105","maxdisk":1,"node":"pve2","status":"running","tags":"a;kubehenge","type":"lxc","vmid":105},{"type":"lxc","vmid":106,"node":"pve3","tags":"kubehenge-test;x"},{"type":"lxc","vmid":107,"node":"pve3"}]
EOF
expect "only a whole tag on a container counts" "105 pve2" appliance_containers
ctid=105
printf 'hostname: kubehenge\ntags: kubehenge\n' > "${root}/config"
expect "a container of another node" "kubehenge: container 105 runs on node pve2. Run this there." require_appliance
node=pve2
expect "a container of this node" "" require_appliance
ctid=101
printf 'tags: kubehenge-old;x\n' > "${root}/config"
expect "a tag that only starts with the name" "kubehenge: container 101 is not Kubehenge's appliance" require_appliance
printf '[]\n' > "${root}/resources"
ctid=102
printf 'tags: web;kubehenge\n' > "${root}/config"
expect "the cluster's list failing: this node's own configuration answers" "" require_appliance
node=pve1

# Quorum.
rm -f "${root}/quorum"
corosync_conf=${root}/no-corosync
expect "a node in no cluster" yes yes_or_no cluster_quorate
printf 'Cluster information\nQuorate:          Yes\n' > "${root}/quorum"
expect "a quorate cluster" yes yes_or_no cluster_quorate
printf 'Quorate:          No\n' > "${root}/quorum"
expect "a cluster without quorum" no yes_or_no cluster_quorate
rm "${root}/quorum"
: > "${root}/corosync"
corosync_conf=${root}/corosync
expect "a cluster whose corosync does not answer" no yes_or_no cluster_quorate

# The node's resolver.
resolv_conf=${root}/resolv
printf 'search example.org\nnameserver 127.0.0.53\n' > "${resolv_conf}"
expect "only loopback" yes yes_or_no loopback_resolver_only
printf 'nameserver   ::1\nnameserver 127.0.0.1\n' > "${resolv_conf}"
expect "only loopback, IPv6 too" yes yes_or_no loopback_resolver_only
printf 'nameserver 127.0.0.1\nnameserver 192.0.2.53\n' > "${resolv_conf}"
expect "one beyond loopback" no yes_or_no loopback_resolver_only
printf 'search example.org\n' > "${resolv_conf}"
expect "none named" no yes_or_no loopback_resolver_only
printf 'nameserver 10.127.0.1\n' > "${resolv_conf}"
expect "an address that only contains 127" no yes_or_no loopback_resolver_only

# The name of the API as the container resolves it.
printf 'pve1\npve.example.org\n' > "${root}/resolvable"
ctid=105
hosts=$(printf 'pve1.example.org\npve.example.org\n192.0.2.11\n')
expect "the first name the container resolves" pve.example.org resolvable_api_host "${hosts}"
printf 'other\n' > "${root}/resolvable"
expect "an address when no name resolves" 192.0.2.11 resolvable_api_host "${hosts}"
expect "none when no name resolves and there is no address" no yes_or_no resolvable_api_host "$(printf 'a.example.org\nb.example.org\n')"

# The network seen from inside the container.
ctid=105 ctip=192.0.2.40
printf '192.0.2.1\n' > "${root}/gateway"
printf '192.0.2.1\n' > "${root}/pinging"
expect "a gateway that answers" "" check_container_gateway
: > "${root}/pinging"
expect "a gateway that does not answer" "kubehenge: container 105 at 192.0.2.40 cannot reach its gateway 192.0.2.1. Check that --vlan matches the network of the address, and that --gateway is on it" check_container_gateway
touch "${root}/noping"
expect "a template without ping" "" check_container_gateway
rm "${root}/noping"
: > "${root}/gateway"
expect "no gateway: nothing to ping" "" check_container_gateway

proxmox_url=https://192.0.2.11:8006
printf '192.0.2.11:8006\n' > "${root}/open"
expect "the API's port open" "" check_container_api_port 192.0.2.11
: > "${root}/open"
printf '192.0.2.1\n' > "${root}/gateway"
expect "the API's port closed" "kubehenge: container 105 cannot connect to the Proxmox VE API at 192.0.2.11 on port 8006. Check the node's firewall for that port and pveproxy's LISTEN_IP in /etc/default/pveproxy." check_container_api_port 192.0.2.11
: > "${root}/gateway"
expect "the API's port closed, no default route" "kubehenge: container 105 cannot connect to the Proxmox VE API at 192.0.2.11 on port 8006. Check the node's firewall for that port and pveproxy's LISTEN_IP in /etc/default/pveproxy. The container has no default route, so give --gateway." check_container_api_port 192.0.2.11
printf 'other\n' > "${root}/resolvable"
expect "no name resolving" "kubehenge: the container cannot resolve any host the Proxmox VE API could be reached at (tried pve.example.org). Give an address its certificate is valid for with --proxmox-url, or name servers the container reaches with --dns" require_resolvable_api_host pve.example.org

# The question about the entry.
answers=${root}/answers
tty=${answers}
printf 'y\n' > "${answers}"
# The question goes to standard error.
entry_answer() { want_pve_entry 2> /dev/null; }
expect "yes on the terminal" yes entry_answer
printf 'n\n' > "${answers}"
expect "no on the terminal" no entry_answer
tty=${root}/no-terminal
expect "no terminal: no" no entry_answer

# The cleanup runs when the session drops.
dropped() {
    sh -c "KUBEHENGE_INSTALL_FUNCTIONS=1; . '${installer}'; install_traps 'echo cleaned'; kill -$1 \$\$; sleep 5; echo survived" 2>&1 | tr -d '\r'
}
expect "a dropped session: the cleanup runs" cleaned dropped HUP
expect "a terminated script: the cleanup runs" cleaned dropped TERM

if [ "${failures}" -ne 0 ]; then
    echo "install-network: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-network: all ${passes} checks passed"
