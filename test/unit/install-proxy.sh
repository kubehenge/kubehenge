#!/bin/sh
# Unit test of how appliance/install.sh uses a proxy: the datacenter's goes to downloads alone,
# never to a request for the container or the Proxmox VE API, and is printed without credentials.
# A stand-in for curl records its arguments. Needs no Proxmox VE.
#
#   test/unit/install-proxy.sh
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
    got=$("$@" 2>&1 || true)
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
    got=$("$@" 2>&1 || true)
    case "${got}" in
        *"${wanted}"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted it to say: %s\n  it said: %s\n' "${what}" "${wanted}" "${got}"
            failures=$((failures + 1))
            ;;
    esac
}
# expect_not_in <what> <text unwanted> <command>...: whether the command's output lacks the text.
expect_not_in() {
    what=$1 unwanted=$2
    shift 2
    got=$("$@" 2>&1 || true)
    case "${got}" in
        *"${unwanted}"*)
            printf 'FAIL: %s\n  it must not say: %s\n  it said: %s\n' "${what}" "${unwanted}" "${got}"
            failures=$((failures + 1))
            ;;
        *) passes=$((passes + 1)) ;;
    esac
}

# curl records its arguments, one a line, and fails like a download that went wrong.
cat > "${bin}/curl" << EOF
#!/bin/sh
printf '%s\n' "\$@" > "${root}/curl-args"
printf '%s' "\${http_proxy:-no-proxy-variable}" > "${root}/curl-env"
exit 7
EOF
chmod +x "${bin}/curl"
PATH=${bin}:${PATH}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"

sum=$(printf x | sha256sum | cut -d ' ' -f 1)
# curl is the stand-in that fails, so the download stops, as the test wants.
# shellcheck disable=SC2310 # the failing download is the case under test.
tried_download() {
    (fetch "https://example.org/file" "${root}/got" "${sum}" "the file") > /dev/null 2>&1 || true
}
args() { tr '\n' ' ' < "${root}/curl-args"; }

# The label.
expect "credentials, path and query are left out" "http://proxy.example.org:3128" proxy_label "http://user:p%40ss@proxy.example.org:3128/path?x=1"
expect "a password holding an at sign and a slash" "http://proxy.example.org:3128" proxy_label "http://user:p@s/s@proxy.example.org:3128/"
expect "no credentials: as given" "https://proxy.example.org:3128" proxy_label "https://proxy.example.org:3128"
expect "no scheme: http" "http://proxy.example.org:3128" proxy_label "proxy.example.org:3128"

# The datacenter's proxy.
printf 'keyboard: de\nhttp_proxy:   http://user:secret@proxy.example.org:3128 \r\nlanguage: en\n' > "${root}/datacenter.cfg"
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY || true
read_download_proxy "${root}/datacenter.cfg" > "${root}/said"
expect "the value, trimmed of spaces and the carriage return" "http://user:secret@proxy.example.org:3128" printf '%s' "${download_proxy}"
expect_in "it says where it downloads through" "downloading through http://proxy.example.org:3128, the HTTP proxy" cat "${root}/said"
expect_not_in "it never prints the credentials" "secret" cat "${root}/said"
expect "no http_proxy in the environment" "" sh -c 'env | grep -i "^https\?_proxy=" || true'

environment_names_one() {
    http_proxy=http://env.example.org:8080
    read_download_proxy "${root}/datacenter.cfg" > /dev/null
    printf '%s' "${download_proxy}"
}
expect "a proxy the environment names wins: none passed on" "" environment_names_one
read_download_proxy "${root}/missing" > /dev/null
expect "no datacenter options: none" "" printf '%s' "${download_proxy}"

# Downloads use it, requests to the container and the API do not.
download_proxy=http://proxy.example.org:3128
tried_download
expect_in "a download goes through the proxy" "--proxy http://proxy.example.org:3128" args
download_proxy=''
tried_download
expect_not_in "without one, no --proxy" "--proxy" args

appliance_realms kubehenge.example.org 192.0.2.40 "${root}/tls.crt" > /dev/null
expect_in "the realms check bypasses every proxy" "--noproxy * " args
expect_in "and reaches the container's address" "--resolve kubehenge.example.org:443:192.0.2.40" args

# Security updates in the container: pct copies what it pushes into $root/ct and fails apt as
# $root/apt-fails says.
mkdir -p "${root}/ct"
cat > "${bin}/pct" << EOF
#!/bin/sh
case "\$1" in
    push) cp "\$3" "${root}/ct/\$(basename "\$4")" ;;
    exec)
        case "\$4" in
            dpkg-query) [ -e "${root}/installed" ] && printf 'install ok installed' ;;
            sh) [ ! -e "${root}/apt-fails" ] || { echo "E: Unable to locate package unattended-upgrades"; exit 100; } ;;
            *) exit 2 ;;
        esac
        ;;
    *) exit 2 ;;
esac
EOF
chmod +x "${bin}/pct"
ctid=900 work=${root}
updates() { security_updates 2>&1; }
download_proxy=http://user:secret@proxy.example.org:3128
expect_in "unattended-upgrades is installed" "unattended-upgrades installed" updates
expect_in "apt goes through the installer's proxy" 'Acquire::https::Proxy "http://user:secret@proxy.example.org:3128";' cat "${root}/ct/80kubehenge-proxy"
expect_in "and runs each day" 'APT::Periodic::Unattended-Upgrade "1";' cat "${root}/ct/20auto-upgrades"
rm -f "${root}/ct/"*
download_proxy=''
updates > /dev/null
expect "without a proxy, apt is given none" "" sh -c "ls '${root}/ct' | grep proxy || true"
touch "${root}/apt-fails"
rm -f "${root}/ct/"*
expect_in "a container without Debian's mirrors is warned about" "container 900 could not install unattended-upgrades" updates
expect_in "with apt's own words" "Unable to locate package" updates
expect "and is not set to run it" "" sh -c "ls '${root}/ct' | grep auto-upgrades || true"
rm "${root}/apt-fails"
touch "${root}/installed"
expect "installed already: left as it is" "" updates

if [ "${failures}" -ne 0 ]; then
    echo "install-proxy: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-proxy: all ${passes} checks passed"
