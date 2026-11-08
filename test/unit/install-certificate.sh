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
certificate self6 '' 'subjectAltName = DNS:kubehenge, IP:2001:db8::40, DNS:bank.example.com'
# OpenSSL prints these in full and in capitals.
certificate forms6 '' 'subjectAltName = DNS:kubehenge, IP:2001:db8:0:0:0:0:0:40, IP:FD00::1, IP:::1, IP:2001:db8:0:1:0:0:0:1, IP:2001:db8:0:0:1:0:0:1, IP:1:0:2:0:3:0:4:0, IP:2001:db8:abcd:1:2:3:4:5, IP:192.0.2.40'

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
# ip -6 lists a temporary address too, which no one reaches the appliance by.
cat > "${bin}/ip" << EOF
#!/bin/sh
case "\$1" in
    -6)
        [ ! -s "${root}/inside6" ] ||
            printf '2: eth0    inet6 fd00:ffff::bad/64 scope global temporary dynamic\n2: eth0    inet6 %s/64 scope global dynamic mngtmpaddr\n' "\$(cat "${root}/inside6")"
        ;;
    *) [ ! -s "${root}/inside" ] || printf '2: eth0    inet %s/24 brd 203.0.113.255 scope global eth0\n' "\$(cat "${root}/inside")" ;;
esac
EOF
cat > "${bin}/getent" << 'EOF'
#!/bin/sh
case "$1 $2" in
    "ahostsv4 kubehenge.example.org") echo '192.0.2.40      STREAM kubehenge.example.org' ;;
    "ahostsv4 bank.example.com") echo '203.0.113.7     STREAM bank.example.com' ;;
    "ahostsv6 kubehenge6.example.org") echo '2001:db8::40   STREAM kubehenge6.example.org' ;;
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

# IPv6: a static address from the configuration, else the global one the container reports. IPv4
# comes first where the container has both.
: > "${root}/inside"
: > "${root}/inside6"
printf 'hostname: kubehenge\nnet0: name=eth0,bridge=vmbr0,ip6=2001:db8::40/64,gw6=2001:db8::1,type=veth\n' > "${root}/config"
expect "IPv6 alone: the address of the configuration" 2001:db8::40 appliance_ip
expect "and it is the IPv6 address" 2001:db8::40 appliance_ip6
expect "and there is no IPv4 one" "" appliance_ip4
printf 'hostname: kubehenge\nnet0: name=eth0,bridge=vmbr0,ip=dhcp,ip6=auto,type=veth\n' > "${root}/config"
printf '203.0.113.7\n' > "${root}/inside"
printf '2001:db8::77\n' > "${root}/inside6"
expect "both families: IPv4 is the address" 203.0.113.7 appliance_ip
expect "an IPv6 address from router advertisement, not the temporary one" 2001:db8::77 appliance_ip6
addresses_joined() { appliance_addresses | paste -sd ' ' -; }
expect "the certificate names both" "203.0.113.7 2001:db8::77" addresses_joined
: > "${root}/inside"
expect "IPv4 missing: the IPv6 address is the address" 2001:db8::77 appliance_ip

# A name the container claims is checked against the family of the appliance's address.
claims '[2001:db8::40]'
expect "an IPv6 address is no name" "" public_name "${certs}/self6.crt" 2001:db8::40
claims kubehenge6.example.org
expect "a name this node's DNS resolves to the appliance's IPv6 address goes in" kubehenge6.example.org public_name "${certs}/self6.crt" 2001:db8::40
expect "a name that leads to IPv6 only is none of an IPv4 appliance" "" public_name "${certs}/self.crt" 192.0.2.40

# The names and addresses of a certificate, an IPv6 address as the kernel writes it.
expect "an IPv6 address of the certificate" "dns kubehenge
ip 2001:db8::40
dns bank.example.com" certificate_names "${certs}/self6.crt"
expect "forms of IPv6 addresses" "dns kubehenge
ip 2001:db8::40
ip fd00::1
ip ::1
ip 2001:db8:0:1::1
ip 2001:db8::1:0:0:1
ip 1:0:2:0:3:0:4:0
ip 2001:db8:abcd:1:2:3:4:5
ip 192.0.2.40" certificate_names "${certs}/forms6.crt"

# The certificate starts an hour back, so that a browser whose clock is behind this node's by
# minutes does not refuse it as not yet valid, and lasts no more than the 825 days browsers accept.
# A leading slash in -subj is a path to Git for Windows' shell.
export MSYS2_ARG_CONV_EXCL='/CN='
# OpenSSL for Windows has no default configuration for req to read.
printf '[req]\ndistinguished_name = dn\n[dn]\n' > "${root}/openssl.cnf"
OPENSSL_CONF=${root}/openssl.cnf
export OPENSSL_CONF
make_certificate kubehenge 192.0.2.40
startdate=$(openssl x509 -in "${work}/tls.crt" -noout -startdate)
enddate=$(openssl x509 -in "${work}/tls.crt" -noout -enddate)
started=$(date -d "${startdate#*=}" +%s)
ended=$(date -d "${enddate#*=}" +%s)
now=$(date +%s)
expect "a new certificate starts between 59 and 61 minutes back" yes sh -c \
    "age=$((now - started)); [ \"\${age}\" -ge 3540 ] && [ \"\${age}\" -le 3660 ] && echo yes"
expect "and ends within 825 days of its start" yes sh -c \
    "span=$((ended - started)); [ \"\${span}\" -le $((825 * 86400)) ] && echo yes"

# Every address of the container, of either family, is in the certificate.
make_certificate kubehenge "192.0.2.40
2001:db8::40" kubehenge.example.org
expect "a certificate names the host, each address and the name given" "dns kubehenge
ip 192.0.2.40
ip 2001:db8::40
dns kubehenge.example.org" certificate_names "${work}/tls.crt"
make_certificate kubehenge 2001:db8::40
expect "an IPv6 address alone" "dns kubehenge
ip 2001:db8::40" certificate_names "${work}/tls.crt"
make_certificate kubehenge 192.0.2.40
expect "an IPv4 address alone, as before" "dns kubehenge
ip 192.0.2.40" certificate_names "${work}/tls.crt"

if [ "${failures}" -ne 0 ]; then
    echo "install-certificate: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-certificate: all ${passes} checks passed"
