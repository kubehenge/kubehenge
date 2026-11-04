#!/bin/sh
# Unit test of seaweedfs_stop, seaweedfs_fatal and seaweedfs_answers in test/e2e/common.sh, with
# stand-ins for weed: a store stopped is gone before the next one starts, and one that stopped on a
# fatal error fails the test at once with it.
#
#   test/unit/seaweedfs.sh
set -eu

here=$(dirname "$0")
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT
# shellcheck source=../e2e/common.sh
. "${here}/../e2e/common.sh"

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
# gone <PID>: whether the process is no more.
gone() {
    if kill -0 "$1" 2> /dev/null; then echo running; else echo gone; fi
}
# fatal_of <directory>: seaweedfs_fatal's finding there, or none.
fatal_of() {
    # shellcheck disable=SC2310 # its failure is the finding, none.
    if fatal=$(seaweedfs_fatal "$1"); then echo "fatal: ${fatal}"; else echo none; fi
}

sh -c 'trap "exit 0" TERM; while :; do sleep 1; done' &
weed=$!
seaweedfs_stop "${weed}"
got=$(gone "${weed}")
expect "a weed that stops when asked is gone once seaweedfs_stop returns" gone "${got}"

sh -c 'trap "" TERM; while :; do sleep 1; done' &
weed=$!
KUBEHENGE_E2E_S3_STOP_TIMEOUT=2
seaweedfs_stop "${weed}"
got=$(gone "${weed}")
expect "a weed that ignores the request is killed after the timeout" gone "${got}"
seaweedfs_stop "${weed}"
expect "stopping a weed already gone is no error" on on

printf 'I1103 20:00:00 master started\nE1103 20:00:01 sts signing key missing\n' > "${root}/s3.log"
got=$(fatal_of "${root}")
expect "errors that leave the store running are not fatal" none "${got}"
got=$(seaweedfs_answers "${root}" echo ran)
expect "and its command runs" ran "${got}"
got=$(fatal_of "${root}/none")
expect "a store that never ran has no fatal error" none "${got}"

printf 'F1103 20:00:02 listen tcp :19333: bind: address already in use\n' >> "${root}/s3.log"
# shellcheck disable=SC2310 # its exit is what is checked.
got=$( (seaweedfs_answers "${root}" echo ran) 2>&1 || echo "exit $?")
expect "a store stopped on a fatal error fails the test with it, without running the command" \
    "FAIL: the S3 store stopped: F1103 20:00:02 listen tcp :19333: bind: address already in use
exit 1" "${got}"

printf 'seaweedfs: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
