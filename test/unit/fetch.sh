#!/bin/sh
# Unit test of scripts/fetch.sh with a stand-in for curl: a release asset comes from GitHub's
# download address while it answers, through the API once it fails, and through the API from then on
# for every asset of the run. Any other address never reaches the API, and an asset the release does
# not have is an error.
#
#   test/unit/fetch.sh
set -eu

fetch=$(cd "$(dirname "$0")/../../scripts" && pwd)/fetch.sh
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# expect <what> <wanted> <got>
expect() {
    if [ "$3" = "$2" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}
# expect_of <what> <wanted> <command...>: expect for what the command prints.
expect_of() {
    what=$1 wanted=$2
    shift 2
    got=$("$@")
    expect "${what}" "${wanted}" "${got}"
}

# The stand-in logs every URL it is asked for, and the Authorization header it is given. Addresses on
# github.com answer 504 while STUB_DOWN is set and example.org always does. The API lists the assets
# "tool" (id 11) and "other" (id 12).
mkdir "${root}/bin"
cat > "${root}/bin/curl" << 'EOF'
#!/bin/sh
out=''
while [ $# -gt 1 ]; do
    case "$1" in
        -o)
            out=$2
            shift
            ;;
        -H)
            printf 'header: %s\n' "$2" >> "${STUB_LOG}"
            shift
            ;;
        *) ;;
    esac
    shift
done
url=$1
printf 'url: %s\n' "${url}" >> "${STUB_LOG}"
# emit <text>: to the -o file, or to the standard output like curl.
emit() {
    if [ -n "${out}" ]; then printf '%s' "$1" > "${out}"; else printf '%s' "$1"; fi
}
case "${url}" in
    https://github.com/*)
        [ -z "${STUB_DOWN}" ] || exit 22
        emit direct
        ;;
    https://api.github.com/repos/o/r/releases/tags/v1)
        emit '{"assets":[{"id":11,"name":"tool"},{"id":12,"name":"other"}]}'
        ;;
    https://api.github.com/repos/o/r/releases/assets/11) emit 'api tool' ;;
    https://api.github.com/repos/o/r/releases/assets/12) emit 'api other' ;;
    *) exit 22 ;;
esac
EOF
chmod +x "${root}/bin/curl"
PATH=${root}/bin:${PATH}
export PATH

log="${root}/log"
state="${root}/state"
# run <STUB_DOWN> <GITHUB_TOKEN> <url> <output>: fetch.sh against the stand-in, its marker private to
# the case. An empty first word means the address answers, an empty second no token.
run() {
    env STUB_LOG="${log}" STUB_DOWN="$1" GITHUB_TOKEN="$2" KUBEHENGE_FETCH_STATE="${state}" "${fetch}" "$3" "$4" 2> "${root}/err"
}
# calls <text>: how many logged lines hold it.
calls() {
    grep -c -F -- "$1" "${log}" || true
}
# absent <file>
absent() {
    if [ -e "$1" ]; then echo present; else echo absent; fi
}
fresh() {
    rm -f "${log}" "${state}" "${root}/out" "${root}/out2"
    : > "${log}"
}

tool=https://github.com/o/r/releases/download/v1/tool
other=https://github.com/o/r/releases/download/v1/other

fresh
run '' '' "${tool}" "${root}/out"
expect_of "the address answering: its file" direct cat "${root}/out"
expect_of "the address answering: no API request" 0 calls api.github.com

fresh
run 1 '' "${tool}" "${root}/out"
expect_of "the address failing: the API's file" "api tool" cat "${root}/out"
expect_of "the address failing: one try of the address" 1 calls 'url: https://github.com/'
expect_of "the address failing: no Authorization header without a token" 0 calls 'header: Authorization:'

fresh
run 1 secret "${tool}" "${root}/out"
expect_of "a token goes to both API requests" 2 calls 'header: Authorization: Bearer secret'

fresh
run 1 '' "${tool}" "${root}/out"
run 1 '' "${other}" "${root}/out2"
expect_of "a second asset after a failure: its API file" "api other" cat "${root}/out2"
expect_of "a second asset after a failure: the address is not tried again" 1 calls 'url: https://github.com/'

fresh
run '' '' "${tool}" "${root}/out"
run 1 '' "${other}" "${root}/out2"
expect_of "a failure after a success: the next asset tried the address" 1 calls 'url: https://github.com/o/r/releases/download/v1/other'

fresh
rc=0
# shellcheck disable=SC2310 # its exit is what is checked.
run 1 '' https://github.com/o/r/releases/download/v1/missing "${root}/out" || rc=$?
got=ok
[ "${rc}" -eq 0 ] || got=failed
expect "an asset the release lacks: fails" failed "${got}"
expect_of "an asset the release lacks: names it" "fetch: missing is not an asset of o/r v1" head -n 1 "${root}/err"
expect_of "an asset the release lacks: leaves no file" absent absent "${root}/out"

fresh
rc=0
# shellcheck disable=SC2310 # its exit is what is checked.
run 1 '' https://example.org/file "${root}/out" || rc=$?
got=ok
[ "${rc}" -eq 0 ] || got=failed
expect "another address failing: fails" failed "${got}"
expect_of "another address failing: no API request" 0 calls api.github.com
expect_of "another address failing: no switch remembered" absent absent "${state}"

# Without KUBEHENGE_FETCH_STATE the marker is a file in the temporary directory, which separate
# processes of one run share.
fresh
plain() {
    env -u KUBEHENGE_FETCH_STATE TMPDIR="${root}" STUB_LOG="${log}" STUB_DOWN=1 "${fetch}" "$1" "$2"
}
plain "${tool}" "${root}/out"
plain "${other}" "${root}/out2"
expect_of "the default marker spans processes" 1 calls 'url: https://github.com/'
expect_of "the default marker leads to the API's file" "api other" cat "${root}/out2"

printf 'fetch: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
