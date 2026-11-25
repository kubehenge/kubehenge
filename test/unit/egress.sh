#!/bin/sh
# Unit test of the assertions in test/e2e/ci/egress.sh on fixture flow logs: a lane that bypassed the
# intercepting proxy, the CONNECT proxy or its exceptions fails, and one that did not passes.
#
#   test/unit/egress.sh
set -eu

here=$(dirname "$0")
egress=${here}/../e2e/ci/egress.sh
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

passes=0 failures=0
ok() { passes=$((passes + 1)); }
bad() {
    printf 'FAIL: %s\n' "$*"
    failures=$((failures + 1))
}
# passes_on <what> <egress.sh arguments>...
passes_on() {
    passes_on_what=$1
    shift
    if sh "${egress}" "$@" > /dev/null 2>&1; then ok; else bad "${passes_on_what}: refused"; fi
}
# fails_on <what> <message> <egress.sh arguments>...: the message must name what is wrong.
fails_on() {
    fails_on_what=$1
    fails_on_message=$2
    shift 2
    if sh "${egress}" "$@" > "${work}/out" 2>&1; then
        bad "${fails_on_what}: accepted"
        return
    fi
    if grep -qF -- "${fails_on_message}" "${work}/out"; then
        ok
    else
        fails_on_said=$(cat "${work}/out")
        bad "${fails_on_what}: said ${fails_on_said} not ${fails_on_message}"
    fi
}

# What mitmproxy prints in transparent mode and as an intercepting proxy: a request inside a
# re-signed connection, and a connection whose client refused the certificate, which holds no request.
cat > "${work}/intercepting.log" << 'EOF'
[18:04:49.334] HTTP(S) proxy listening at *:8080.
[18:04:53.975][172.20.99.11:42846] client connect
[18:04:54.013][172.20.99.11:42846] server connect registry.k8s.io:443 (34.96.108.209:443)
172.20.99.11:42846: GET https://registry.k8s.io/v2/ HTTP/2.0
     << HTTP/2.0 200 OK 577b
[18:04:54.072][172.20.99.11:42846] client disconnect
[18:04:54.092][172.20.99.21:42854] client connect
[18:04:54.099][172.20.99.21:42854] server connect ghcr.io:443 (140.82.121.34:443)
172.20.99.21:42854: GET https://ghcr.io/v2/ HTTP/1.1
     << HTTP/1.1 403 Forbidden 0b
[18:04:54.100][172.20.99.40:42900] client connect
[18:04:54.101][172.20.99.40:42900] server connect github.com:443 (140.82.121.3:443)
[18:04:54.119][172.20.99.40:42900] client disconnect
[18:04:54.120][172.20.99.50:42901] client connect
[18:04:54.121][172.20.99.50:42901] server connect deb.debian.org:443 (151.101.2.132:443)
172.20.99.50:42901: POST https://deb.debian.org/x HTTP/2.0
EOF
passes_on "nodes and the build address, all re-signed" intercepted "${work}/intercepting.log" '172\.20\.99\.1[1-3]' '172\.20\.99\.2[1-9]' '172\.20\.99\.50'
fails_on "an appliance whose client refused the certificate" '172\.20\.99\.40 made no HTTPS request' \
    intercepted "${work}/intercepting.log" '172\.20\.99\.40'
fails_on "a source that never connected" '172\.20\.99\.12 made no HTTPS request' \
    intercepted "${work}/intercepting.log" '172\.20\.99\.11' '172\.20\.99\.12'
# Only a request in plain HTTP, or the address inside a longer one, is no proof.
printf '172.20.99.11:1: GET http://example.com/ HTTP/1.1\n1172.20.99.12:1: GET https://example.com/ HTTP/1.1\n' > "${work}/plain.log"
fails_on "a plain HTTP request" '172\.20\.99\.11 made no HTTPS request' intercepted "${work}/plain.log" '172\.20\.99\.11'
fails_on "an address inside a longer one" '172\.20\.99\.12 made no HTTPS request' intercepted "${work}/plain.log" '172\.20\.99\.12'
# The check holds an empty log to be a bypass too.
: > "${work}/empty.log"
fails_on "a log with nothing in it" 'made no HTTPS request' intercepted "${work}/empty.log" '172\.20\.99\.11'

# A CONNECT proxy that tunnels: no request lines, only the connections it opened, and what it refused.
cat > "${work}/tunnel.log" << 'EOF'
[18:05:00.563][172.20.99.11:45624] client connect
[18:05:00.571][172.20.99.11:45624] server connect registry.k8s.io:443 (34.96.108.209:443)
[18:05:00.593][172.20.99.11:45624] server disconnect registry.k8s.io:443 (34.96.108.209:443)
[18:05:00.606][172.20.99.40:45632] client connect
172.20.99.40:45632: CONNECT example.com:443
              << 407 Proxy Authentication Required 129b
[18:05:00.700][172.20.99.50:45700] client connect
[18:05:00.701][172.20.99.50:45700] server connect deb.debian.org:80 (151.101.2.132:80)
EOF
passes_on "nodes and the build address through the tunnel" tunnelled "${work}/tunnel.log" '172\.20\.99\.1[1-3]' '172\.20\.99\.50'
fails_on "an appliance the proxy refused" '172\.20\.99\.40 opened no connection through the proxy' \
    tunnelled "${work}/tunnel.log" '172\.20\.99\.40'
fails_on "a worker that never used the proxy" '172\.20\.99\.21 opened no connection through the proxy' \
    tunnelled "${work}/tunnel.log" '172\.20\.99\.21'
passes_on "a proxy asked for external hosts alone" internal "${work}/tunnel.log"
for host in 172.21.99.2:8006 pve:8006 172.20.99.11:6443 10.244.0.5:80 192.168.1.1:443 127.0.0.1:8080 172.21.99.3:5000; do
    printf '%s\n' "[18:05:01.000][172.20.99.11:45700] server connect ${host} (1.2.3.4:1)" > "${work}/internal.log"
    cat "${work}/tunnel.log" >> "${work}/internal.log"
    fails_on "a request for ${host}" "${host%%:*}" internal "${work}/internal.log"
done
# Hosts that only start like an internal one are external.
printf '%s\n' '[18:05:01.000][172.20.99.11:1] server connect pve.example.com:443 (1.2.3.4:443)' \
    '[18:05:01.000][172.20.99.11:1] server connect 172.32.0.1:443 (1.2.3.4:443)' \
    '[18:05:01.000][172.20.99.11:1] server connect 110.1.1.1:443 (1.2.3.4:443)' > "${work}/external.log"
passes_on "external hosts named like internal ones" internal "${work}/external.log"

# No file holds the password.
printf 'hunter2xyz' > "${work}/secret"
printf 'GET https://example.com/\n' > "${work}/clean.log"
printf 'proxy password hunter2xyz for e2e\n' > "${work}/dirty.log"
passes_on "logs without the password" leaks "${work}/secret" "${work}/clean.log" "${work}/missing.log"
fails_on "a log with the password" "${work}/dirty.log holds the proxy's password" \
    leaks "${work}/secret" "${work}/clean.log" "${work}/dirty.log"
passes_on "no password at all" leaks "${work}/nothing" "${work}/dirty.log"

# The kind is one of the four, and the run's directory holds the flow log check reads.
fails_on "an unknown kind" 'unknown kind tls' check "${work}" tls '172\.20\.99\.11'
fails_on "no arguments" 'usage:' start
mkdir "${work}/egress"
cp "${work}/intercepting.log" "${work}/egress/flows.log"
passes_on "check of an inspecting lane" check "${work}" tls-inspection '172\.20\.99\.11' '172\.20\.99\.50'
fails_on "check of a bypassing source" '172\.20\.99\.40 made no HTTPS request' check "${work}" tls-inspection-recovery '172\.20\.99\.40'
cp "${work}/tunnel.log" "${work}/egress/flows.log"
passes_on "check of a tunnelling lane" check "${work}" proxy '172\.20\.99\.11'
fails_on "check of a tunnelling lane that bypassed" '172\.20\.99\.12 opened no connection' check "${work}" proxy '172\.20\.99\.12'
cp "${work}/internal.log" "${work}/egress/flows.log"
fails_on "check of a proxy asked for an internal address" 'asked for internal addresses' check "${work}" proxy '172\.20\.99\.11'
cp "${work}/intercepting.log" "${work}/egress/flows.log"
printf 'hunter2xyz' > "${work}/egress/secret"
printf 'password hunter2xyz' > "${work}/apply.log"
fails_on "check of a lane whose log holds the password" 'apply.log holds the proxy' check "${work}" proxy-inspecting '172\.20\.99\.11'

printf 'egress: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
