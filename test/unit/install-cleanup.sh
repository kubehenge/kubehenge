#!/bin/sh
# Unit test of what appliance/install.sh removes after a failure: its cleanup, run against a
# stand-in for pct that records what it is asked. Needs no Proxmox VE.
#
#   test/unit/install-cleanup.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# asked <what> <wanted>: whether pct was asked exactly the lines of <wanted>, in order.
asked() {
    got=$(cat "${root}/pct.log" 2> /dev/null || true)
    if [ "${got}" = "$2" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted pct to be asked:\n%s\n  it was asked:\n%s\n' "$1" "$2" "${got}"
        failures=$((failures + 1))
    fi
    rm -f "${root}/pct.log"
}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"

bin=${root}/bin
mkdir "${bin}"
printf '#!/bin/sh\necho "$*" >> "%s/pct.log"\n' "${root}" > "${bin}/pct"
chmod +x "${bin}/pct"
PATH=${bin}:${PATH}

# fails_then_cleans: cleanup as the exit trap runs it after a failure.
fails_then_cleans() {
    work=$(mktemp -d "${root}/work.XXXXXX")
    sh -c 'exit 1' || cleanup 2> /dev/null
}

ctid=991 started='' partial=''

# A container whose pct create was cut off is left locked, with a disk its config may not name yet.
created=1
fails_then_cleans
asked "a container whose creation was cut off" "unlock 991
stop 991
destroy 991 --purge --destroy-unreferenced-disks 1"

created=''
fails_then_cleans
asked "a failure before the container" ""

created=1
work=$(mktemp -d "${root}/work.XXXXXX")
true && cleanup
asked "a successful install" ""
if [ -d "${work}" ]; then
    echo "FAIL: the work directory stays after a successful install"
    failures=$((failures + 1))
else
    passes=$((passes + 1))
fi

if [ "${failures}" -ne 0 ]; then
    echo "install-cleanup: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-cleanup: all ${passes} checks passed"
