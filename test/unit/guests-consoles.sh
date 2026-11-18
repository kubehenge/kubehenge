#!/bin/sh
# Unit test of how test/e2e/ci/guests.sh reads the VMs' serial consoles: a stand-in for docker
# (Proxmox VE's qm and socat) with one VM in the first container and no second container, and a
# stand-in sleep that ends the third round. Needs no Proxmox VE.
#
#   test/unit/guests-consoles.sh
set -eu

here=$(dirname "$0")
guests=${here}/../e2e/ci/guests.sh
root=$(mktemp -d)
bin=${root}/bin
mkdir "${bin}"
trap 'rm -f "${root}/hold"; rm -rf "${root}"' EXIT
real_sleep=$(command -v sleep)

passes=0 failures=0
# same <what> <wanted> <got>
same() {
    if [ "$2" = "$3" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}

# The console prints a line with a token in it, then stays open while hold exists, as a running
# VM's does.
cat > "${bin}/docker" << EOF
#!/bin/sh
case "\$*" in
    "exec kubehenge-e2e-pve2 "*) exit 1 ;;
    "exec kubehenge-e2e-pve sh -c "*) echo 101.serial0 ;;
    "exec kubehenge-e2e-pve qm config 101") printf 'cores: 2\nname: cp\n' ;;
    "exec kubehenge-e2e-pve socat -u UNIX-CONNECT:/var/run/qemu-server/101.serial0 STDOUT")
        echo socat >> "${root}/asked"
        printf 'dracut: FATAL: FIPS integrity test failed\ntoken: abcdefgh12345678\n'
        i=0
        while [ -f "${root}/hold" ] && [ "\${i}" -lt 100 ]; do
            "${real_sleep}" 0.1
            i=\$((i + 1))
        done
        ;;
    *) exit 1 ;;
esac
EOF
cat > "${bin}/sleep" << EOF
#!/bin/sh
rounds=0
[ ! -f "${root}/rounds" ] || read -r rounds < "${root}/rounds"
rounds=\$((rounds + 1))
echo "\${rounds}" > "${root}/rounds"
[ "\${rounds}" -lt 3 ]
EOF
chmod +x "${bin}"/*

# run: guests.sh consoles for three rounds, then waits for the console's reader to end once hold
# is gone.
run() {
    rm -f "${root}/rounds"
    env PATH="${bin}:${PATH}" sh "${guests}" consoles "${root}/consoles" > /dev/null 2>&1 || true
    rm -f "${root}/hold"
    i=0
    while [ -s "${root}/consoles/pve-101.log.pid" ] && kill -0 "$(cat "${root}/consoles/pve-101.log.pid")" 2> /dev/null &&
        [ "${i}" -lt 100 ]; do
        sleep 0.1
        i=$((i + 1))
    done
}
asks() { if [ -f "${root}/asked" ]; then grep -c . "${root}/asked"; else echo 0; fi; }

touch "${root}/hold"
run
same "a console stays with its one reader over the rounds" 1 "$(asks)"
same "each reading starts with the time and the VM's name" 1 "$(grep -c '^== [0-9][0-9]:[0-9][0-9]:[0-9][0-9] cp$' "${root}/consoles/pve-101.log")"
same "what the console printed is kept" 1 "$(grep -cx 'dracut: FATAL: FIPS integrity test failed' "${root}/consoles/pve-101.log")"
same "a token on the console is redacted" 1 "$(grep -cx 'token: <redacted>' "${root}/consoles/pve-101.log")"
same "a container that is not there gives no log" "pve-101.log pve-101.log.pid" "$(cd "${root}/consoles" && echo *)"

touch "${root}/hold"
run
same "a console whose reader ended is read again" 2 "$(asks)"
same "the second reading follows the first in the same log" 2 "$(grep -c '^== ' "${root}/consoles/pve-101.log")"

echo "guests-consoles: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
