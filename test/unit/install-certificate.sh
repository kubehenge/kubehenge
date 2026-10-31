#!/bin/sh
# Unit test of which name and address appliance/install.sh puts into a certificate it renews or
# replaces for an installed appliance: those this node vouches for, never a name the container
# alone claims. Stand-ins answer for pct and DNS. Needs openssl and no Proxmox VE.
#
#   test/unit/install-certificate.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
certs=${root}/certs bin=${root}/bin
mkdir "${certs}" "${bin}" "${root}/work"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# expect <what> <wanted> <command>...: whether the command prints what is wanted.
expect() {
    what=$1 wanted=$2
    shift 2
    got=$("$@" 2> /dev/null || true)
    if [ "${got}" = "${wanted}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "${what}" "${wanted}" "${got}"
        failures=$((failures + 1))
    fi
}

# certificate <name> <issuer, or "" for self-signed> <subject alternative names>: $certs/<name>.crt
# and .key. A configuration file rather than -subj, which some shells rewrite as a path.
certificate() {
    printf '[req]\nprompt = no\ndistinguished_name = dn\n[dn]\nCN = %s\n[ext]\n%s\n' "$1" "$3" > "${certs}/$1.cnf"
    openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -config "${certs}/$1.cnf" \
        -keyout "${certs}/$1.key" -out "${certs}/$1.csr" 2> /dev/null
    if [ -z "$2" ]; then
        openssl x509 -req -in "${certs}/$1.csr" -signkey "${certs}/$1.key" -days 30 \
            -extfile "${certs}/$1.cnf" -extensions ext -out "${certs}/$1.crt" 2> /dev/null
    else
        openssl x509 -req -in "${certs}/$1.csr" -CA "${certs}/$2.crt" -CAkey "${certs}/$2.key" \
            -CAcreateserial -CAserial "${certs}/serial" -days 30 -extfile "${certs}/$1.cnf" -extensions ext \
            -out "${certs}/$1.crt" 2> /dev/null
    fi
}
certificate cluster-ca '' 'basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign'
# As make_certificate signs them: the host name, the address, and the name given with --name.
certificate signed cluster-ca 'subjectAltName = DNS:kubehenge, IP:192.0.2.40, DNS:kubehenge.example.org'
certificate signed-unnamed cluster-ca 'subjectAltName = DNS:kubehenge, IP:192.0.2.40'
certificate self '' 'subjectAltName = DNS:kubehenge, IP:192.0.2.40, DNS:bank.example.com'

# The container's configuration on this node, and what runs inside it: its settings, which its
# root may have rewritten, and its own address.
cat > "${bin}/pct" << EOF
#!/bin/sh
case "\$1" in
    config) cat "${root}/config" ;;
    exec)
        shift 3
        for arg; do
            shift
            [ "\${arg}" = /etc/kubehenge/appliance.env ] && arg=${root}/public
            set -- "\$@" "\${arg}"
        done
        "\$@"
        ;;
    *) exit 2 ;;
esac
EOF
cat > "${bin}/ip" << EOF
#!/bin/sh
printf '2: eth0    inet %s/24 brd 203.0.113.255 scope global eth0\n' "\$(cat "${root}/inside")"
EOF
cat > "${bin}/getent" << 'EOF'
#!/bin/sh
case "$2" in
    kubehenge.example.org) echo '192.0.2.40      STREAM kubehenge.example.org' ;;
    bank.example.com) echo '203.0.113.7     STREAM bank.example.com' ;;
    *) exit 2 ;;
esac
EOF
chmod +x "${bin}/pct" "${bin}/getent" "${bin}/ip"
PATH=${bin}:${PATH}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"
ctid=900 work=${root}/work
cluster_ca=${certs}/cluster-ca.crt

name_for() { public_name "${certs}/$1.crt" 192.0.2.40; }
claims() { printf 'KUBEHENGE_PUBLIC_URL=https://%s\n' "$1" > "${root}/public"; }

claims bank.example.com
expect "a certificate this cluster's CA signed keeps its name, whatever the container claims" kubehenge.example.org name_for signed
expect "and without a name it gets none" "" name_for signed-unnamed
expect "a name the container alone claims stays out" "" name_for self
claims kubehenge.example.org
expect "a name this node's DNS resolves to the appliance goes in" kubehenge.example.org name_for self
claims 192.0.2.40
expect "an address is no name" "" name_for self
claims 'kubehenge.example.org, DNS:bank.example.com'
expect "a list of names is none" "" name_for self

printf 'hostname: kubehenge\nnet0: name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=192.0.2.40/24,gw=192.0.2.1,type=veth\n' > "${root}/config"
printf '203.0.113.7\n' > "${root}/inside"
expect "a static address comes from the container's configuration, not from inside it" 192.0.2.40 appliance_ip
printf 'hostname: kubehenge\nnet0: name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp,type=veth\n' > "${root}/config"
expect "an address from DHCP is the one the container has" 203.0.113.7 appliance_ip

if [ "${failures}" -ne 0 ]; then
    echo "install-certificate: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-certificate: all ${passes} checks passed"
