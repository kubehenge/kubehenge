#!/bin/sh
# Unit test of how appliance/install.sh chooses the Proxmox VE API's address and the CA it checks
# the API against: certificates made here, a TLS server on loopback, and stand-ins for the
# commands that would ask Proxmox VE and DNS. Needs openssl and no Proxmox VE.
#
#   test/unit/install-api.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
certs=${root}/certs bin=${root}/bin
mkdir "${certs}" "${bin}" "${root}/work"
server=''
finish() {
    [ -z "${server}" ] || kill "${server}" 2> /dev/null || true
    rm -rf "${root}"
}
trap finish EXIT

passes=0 failures=0
# expect <what> <wanted> <command>...: whether the command prints what is wanted.
expect() {
    what=$1 wanted=$2
    shift 2
    got=$("$@" || true)
    if [ "${got}" = "${wanted}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "${what}" "${wanted}" "${got}"
        failures=$((failures + 1))
    fi
}
# expect_in <what> <text wanted> <command>...: whether the command's output holds the text.
expect_in() {
    what=$1 wanted=$2
    shift 2
    got=$("$@" || true)
    case "${got}" in
        *"${wanted}"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted it to say: %s\n  it said: %s\n' "${what}" "${wanted}" "${got}"
            failures=$((failures + 1))
            ;;
    esac
}

# certificate <name> <common name> <issuer, or "" for self-signed> <extensions>: $certs/<name>.crt
# and .key. A configuration file rather than -subj, which some shells rewrite as a path.
certificate() {
    printf '[req]\nprompt = no\ndistinguished_name = dn\n[dn]\nO = Example\nCN = %s\n[ext]\n%s\n' "$2" "$4" > "${certs}/$1.cnf"
    openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -config "${certs}/$1.cnf" \
        -keyout "${certs}/$1.key" -out "${certs}/$1.csr" 2> /dev/null
    if [ -z "$3" ]; then
        openssl x509 -req -in "${certs}/$1.csr" -signkey "${certs}/$1.key" -days 30 \
            -extfile "${certs}/$1.cnf" -extensions ext -out "${certs}/$1.crt" 2> /dev/null
    else
        openssl x509 -req -in "${certs}/$1.csr" -CA "${certs}/$3.crt" -CAkey "${certs}/$3.key" \
            -CAcreateserial -CAserial "${certs}/serial" -days 30 -extfile "${certs}/$1.cnf" -extensions ext \
            -out "${certs}/$1.crt" 2> /dev/null
    fi
}
ca='basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign'
leaf='basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth'

certificate org-root "Example Org Root CA" "" "${ca}"
certificate org-issuing "Example Org Issuing CA" org-root "${ca}"
certificate other-root "Example Other Root CA" "" "${ca}"
certificate cluster-root "Example Cluster CA" "" "${ca}"
# The node's certificate from an organisation's CA, valid for hosts outside the cluster too, the
# first it names among them.
certificate org-node pve1.example.org org-issuing "${leaf}
subjectAltName = DNS:backup.example.org, IP:192.0.2.50, DNS:pve1.example.org, IP:127.0.0.1, IP:192.0.2.11"
certificate cluster-node pve1.example.org cluster-root "${leaf}
subjectAltName = DNS:pve1.example.org, IP:127.0.0.1"
certificate wildcard '*.example.org' org-issuing "${leaf}
subjectAltName = DNS:backup.example.org, DNS:*.example.org"
certificate outside backup.example.org org-issuing "${leaf}
subjectAltName = DNS:backup.example.org, IP:192.0.2.50"
certificate cluster-names pve.example.org org-issuing "${leaf}
subjectAltName = DNS:pve.example.org, DNS:api.example.org, DNS:pve1.example.org, IP:192.0.2.11"
certificate addresses 192.0.2.11 org-issuing "${leaf}
subjectAltName = IP:192.0.2.50, IP:198.51.100.11, IP:192.0.2.11"
certificate short pve1 org-issuing "${leaf}
subjectAltName = DNS:localhost, DNS:pve1, IP:127.0.0.1, IP:198.51.100.11, DNS:pve2.example.org"
certificate mixed mixed.example.org org-issuing "${leaf}
subjectAltName = DNS:mixed.example.org, IP:192.0.2.12, DNS:PVE2.example.org"

# This node, pve1, on 192.0.2.11 (its bridge) and 198.51.100.11; pve2 and pve3 beside it; a
# backup server outside the cluster; and names for the whole cluster.
cat > "${root}/hosts" << 'EOF'
127.0.0.1 localhost
192.0.2.11 pve1.example.org pve1 pve.example.org api.example.org mixed.example.org
192.0.2.12 pve2.example.org pve2 pve.example.org api.example.org
192.0.2.13 pve3.example.org pve3 pve.example.org api.example.org
192.0.2.50 backup.example.org mixed.example.org
EOF
cat > "${bin}/getent" << EOF
#!/bin/sh
[ "\$1" = ahostsv4 ] || exit 2
awk -v name="\$2" '{ for (i = 2; i <= NF; i++) if (tolower(\$i) == tolower(name)) { print \$1 "      STREAM " name; found = 1 } } END { exit !found }' "${root}/hosts"
EOF
cat > "${bin}/ip" << 'EOF'
#!/bin/sh
printf '1: lo    inet 127.0.0.1/8 scope host lo\n'
printf '2: vmbr0    inet 192.0.2.11/24 brd 192.0.2.255 scope global vmbr0\n'
printf '3: vmbr1    inet 198.51.100.11/24 brd 198.51.100.255 scope global vmbr1\n'
EOF
cat > "${bin}/pvesh" << 'EOF'
#!/bin/sh
printf '[{"id":"cluster","name":"example","nodes":3,"quorate":1,"type":"cluster","version":3},'
printf '{"id":"node/pve1","ip":"192.0.2.11","level":"","local":1,"name":"pve1","nodeid":1,"online":1,"type":"node"},'
printf '{"id":"node/pve2","ip":"192.0.2.12","level":"","local":0,"name":"pve2","nodeid":2,"online":1,"type":"node"},'
printf '{"id":"node/pve3","ip":"192.0.2.13","level":"","local":0,"name":"pve3","nodeid":3,"online":0,"type":"node"}]\n'
EOF
chmod +x "${bin}/getent" "${bin}/ip" "${bin}/pvesh"
PATH=${bin}:${PATH}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"
# The questions are asked on a file holding the answers, or on none.
work=${root}/work node=pve1 bridge=vmbr0 proxmox_ca='' tty=${root}/no-terminal
cluster_ca=${certs}/cluster-root.crt
said() { cat "${root}/said"; }

# The API's address.
members=$(cluster_addresses | grep -v '^127\.' | sort -u)
listed() { printf '%s\n' "${members}" | paste -sd , -; }
expect "the cluster's nodes" "192.0.2.11 pve1,192.0.2.12 pve2,192.0.2.13 pve3,198.51.100.11 pve1" listed

# choose <certificate> [<bridge address> [<FQDN>]]
choose() { choose_api_host "${certs}/$1.crt" "${2-192.0.2.11}" "${3-pve1.example.org}" 2> "${root}/said"; }
# candidates <certificate>
candidates() { api_candidates "${certs}/$1.crt" 192.0.2.11 pve1.example.org | cut -d ' ' -f 2 | paste -sd , -; }
expect "a certificate whose first name is another host's: this node's name" pve1.example.org choose org-node
expect "only names and addresses of this cluster are candidates" "pve1.example.org,192.0.2.11" candidates org-node
expect "another name of this node, without its own" pve1.example.org choose org-node 192.0.2.11 pve1.other.example
expect "addresses alone: this node's on the bridge first" "192.0.2.11,198.51.100.11" candidates addresses
expect "a certificate of no node of the cluster: none" "" choose outside
expect "a wildcard covering this node's name: that name" pve1.example.org choose wildcard
expect "a wildcard not covering this node's name: none" "" choose wildcard 192.0.2.11 pve1.other.example
expect "a wildcard and a name without a domain: none" "" choose wildcard 192.0.2.11 pve1
expect "a name also leading outside the cluster passed over, another node's name taken" pve2.example.org choose mixed
expect "a name without a domain after this node's addresses, loopback never" "198.51.100.11,pve1,pve2.example.org" candidates short

# Several names as good.
expect "without a terminal, the first" pve.example.org choose cluster-names
expect_in "without a terminal, it says which and what else" "at pve.example.org; its certificate is also valid for api.example.org" said
tty=${root}/answers
printf '2\n' > "${tty}"
expect "picked on the terminal" api.example.org choose cluster-names
expect_in "the terminal lists them" "2) api.example.org: a name of nodes pve1, pve2, pve3" said
printf '\n' > "${tty}"
expect "the first, by default" pve.example.org choose cluster-names
printf '9\nnone\n3\n' > "${tty}"
expect "asked again until the answer is one of them" pve1.example.org choose cluster-names
tty=${root}/no-terminal

# The API's CA.
# Not $port, which the installer's functions set.
tls_port=$((20000 + $$ % 20000))
proxmox_url=https://127.0.0.1:${tls_port}
# serve <certificate> [<chain file>]: a TLS server on $proxmox_url with that certificate.
serve() {
    [ -z "${server}" ] || kill "${server}" 2> /dev/null || true
    [ -z "${server}" ] || wait "${server}" 2> /dev/null || true
    set -- -cert "${certs}/$1.crt" -key "${certs}/$1.key" ${2:+-cert_chain "$2"}
    openssl s_server -accept "127.0.0.1:${tls_port}" "$@" -www -quiet < /dev/null > /dev/null 2>&1 &
    server=$!
    for _ in $(seq 1 50); do
        timeout 5 openssl s_client -connect "127.0.0.1:${tls_port}" < /dev/null > /dev/null 2>&1 && return
        sleep 0.1
    done
    echo "the TLS server did not start" >&2
    exit 1
}
# Without the carriage return an openssl built for Windows ends its lines with.
fingerprint() { openssl x509 -in "$1" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d '\r'; }
subject() { openssl x509 -in "$1" -noout -subject | sed 's/^subject=//' | tr -d '\r'; }
# chosen_ca: the fingerprint of the CA file choose_api_ca settles on and how many certificates it
# holds, or what it stopped with.
chosen_ca() {
    # choose_api_ca exits where it stops, which the subshell keeps to itself.
    # shellcheck disable=SC2310 # the subshell's status is the answer.
    if (
        choose_api_ca 2> "${root}/said"
        fingerprint "${api_ca}" > "${root}/fingerprint"
        grep -c -e '-----BEGIN CERTIFICATE-----' "${api_ca}" > "${root}/count"
    ); then
        read -r chosen < "${root}/fingerprint"
        read -r count < "${root}/count"
        echo "${chosen} ${count}"
    else
        said
    fi
}
chosen_file() {
    choose_api_ca 2> /dev/null
    echo "${api_ca}"
}
cat "${certs}/org-issuing.crt" > "${root}/issuing.pem"
cat "${certs}/org-issuing.crt" "${certs}/org-root.crt" > "${root}/issuing-root.pem"
cat "${certs}/other-root.crt" "${certs}/org-root.crt" > "${root}/system-store.pem"
cat "${certs}/other-root.crt" "${certs}/org-issuing.crt" "${certs}/org-root.crt" > "${root}/bundle.pem"
org_root=$(fingerprint "${certs}/org-root.crt")
org_issuing=$(fingerprint "${certs}/org-issuing.crt")
org_node=$(fingerprint "${certs}/org-node.crt")
org_root_subject=$(subject "${certs}/org-root.crt")

serve cluster-node
system_ca=${certs}/other-root.crt
expect "the cluster's CA where it verifies the API" "${cluster_ca}" chosen_file

serve org-node "${root}/issuing.pem"
system_ca=${root}/system-store.pem
expect "an organisation's CA in this node's trust store: that CA alone" "${org_root} 1" chosen_ca
expect_in "it says which CA" "rests on ${org_root_subject} in this node's trust store" said

system_ca=${certs}/other-root.crt
proxmox_ca=${root}/bundle.pem
expect "a bundle given with --proxmox-ca: the CA in it the chain rests on" "${org_root} 1" chosen_ca
proxmox_ca=${certs}/org-root.crt
expect "a CA given with --proxmox-ca" "${org_root} 1" chosen_ca
proxmox_ca=${certs}/other-root.crt
expect_in "a CA given with --proxmox-ca that did not sign it" "serves no certificate that ${proxmox_ca} signed" chosen_ca
proxmox_ca=''

expect_in "a CA nothing here trusts, without a terminal: stops, naming the option and the fingerprint" \
    "SHA-256 fingerprint ${org_issuing}; once you have checked that this is your CA, give it with --proxmox-ca" chosen_ca
tty=${root}/answers
printf 'y\n' > "${tty}"
expect "a CA nothing here trusts, said yes to: the topmost CA served, not self-signed" "${org_issuing} 1" chosen_ca
printf 'n\n' > "${tty}"
expect_in "a CA nothing here trusts, said no to: stops" "serves a certificate this cluster's CA did not sign for it" chosen_ca

serve org-node "${root}/issuing-root.pem"
printf 'yes\n' > "${tty}"
expect "the root served, said yes to: that root" "${org_root} 1" chosen_ca
expect_in "it shows the CA" "Subject: ${org_root_subject}" said

serve org-node
printf 'y\n' > "${tty}"
expect "the certificate alone served, said yes to: that certificate" "${org_node} 1" chosen_ca
tty=${root}/no-terminal

if [ "${failures}" -ne 0 ]; then
    echo "install-api: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-api: all ${passes} checks passed"
