#!/bin/sh
# Unit test of how test/e2e/ci/guests.sh has the guests drop their page cache while memory is short:
# stand-ins for docker (Proxmox VE's qm), pgrep, date and sleep, two VMs of which one runs no
# commands, and the machine's memory. A stand-in clock moves 15 s each time record sleeps. Needs no
# Proxmox VE.
#
#   test/unit/guests-trim.sh
set -eu

here=$(dirname "$0")
guests=${here}/../e2e/ci/guests.sh
root=$(mktemp -d)
bin=${root}/bin
mkdir "${bin}" "${root}/proc" "${root}/proc/7101" "${root}/proc/7102"
trap 'rm -rf "${root}"' EXIT

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

printf -- '-id\000101\000-name\000cp\000' > "${root}/proc/7101/cmdline"
printf -- '-id\000102\000-name\000talos\000' > "${root}/proc/7102/cmdline"
memory() { printf 'MemTotal: 16000000 kB\nMemAvailable: %s kB\n' "$(($1 * 1024))" > "${root}/proc/meminfo"; }

# Each ask goes to asked; VM 102 refuses, as Talos's agent runs no commands.
cat > "${bin}/docker" << EOF
#!/bin/sh
echo "\$*" >> "${root}/asked"
case "\$*" in *" exec 102 "*) exit 1 ;; esac
EOF
cat > "${bin}/pgrep" << 'EOF'
#!/bin/sh
printf '7101\n7102\n'
EOF
cat > "${bin}/date" << EOF
#!/bin/sh
now=\$(cat "${root}/now")
case "\$*" in
    *+%s*) echo "\${now}" ;;
    *) printf '00:%02d:%02d\n' \$(((now / 60) % 60)) \$((now % 60)) ;;
esac
EOF
# Ends record after ten rounds.
cat > "${bin}/sleep" << EOF
#!/bin/sh
echo \$((\$(cat "${root}/now") + 15)) > "${root}/now"
rounds=\$((\$(cat "${root}/rounds" 2> /dev/null || echo 0) + 1))
echo "\${rounds}" > "${root}/rounds"
[ "\${rounds}" -lt 10 ] || kill "\${PPID}"
EOF
chmod +x "${bin}"/*

# run <MiB available> <command> [<environment>...]: guests.sh <command> on a fresh record, from 60
# seconds past the hour.
run() {
    memory "$1"
    echo 60 > "${root}/now"
    rm -f "${root}/asked" "${root}/rounds" "${root}/guests.txt" "${root}/guests.txt.trims"
    command=$2
    shift 2
    # The stand-in sleep ends record, and the shell reports that on its own error output.
    (env PATH="${bin}:${PATH}" KUBEHENGE_E2E_GUESTS_PROC="${root}/proc" "$@" \
        sh "${guests}" "${command}" "${root}/guests.txt" > /dev/null 2>&1) 2> /dev/null || true
}
# Sets asks to the number of asks, first_ask to the first and trims to what was noted.
read_back() {
    asks=0 first_ask='' trims=''
    if [ -f "${root}/asked" ]; then
        asks=$(wc -l < "${root}/asked" | tr -d ' ')
        first_ask=$(sed -n 1p "${root}/asked")
    fi
    [ ! -f "${root}/guests.txt.trims" ] || trims=$(cat "${root}/guests.txt.trims")
}

run 1500 trim
read_back
same "short of memory, each running guest is asked once" 2 "${asks}"
same "the ask drops the page cache through the guest agent" \
    "exec kubehenge-e2e-pve qm guest exec 101 --timeout 20 -- sh -c echo 1 > /proc/sys/vm/drop_caches" \
    "${first_ask}"
same "only the guest that dropped it is noted, with the MiB available" "00:01:00 101 1500" "${trims}"

run 4096 trim
read_back
same "with memory to spare, no guest is asked" 0 "${asks}"

run 1500 trim KUBEHENGE_E2E_TRIM_BELOW=0
read_back
same "0 turns it off" 0 "${asks}"

# Ten rounds, 15 s apart: from 60 s to 195 s. Each guest is asked at 60 and again at 180.
run 1500 record
read_back
same "record asks each guest again only after 2 minutes" 4 "${asks}"
same "record notes each time the guest dropped it" "00:01:00 101 1500
00:03:00 101 1500" "${trims}"
summary=$(sh "${guests}" summary "${root}/guests.txt" 2>&1 || true)
line=$(printf '%s\n' "${summary}" | grep '^  101:' || true)
same "the summary names the guest that dropped its cache" \
    "  101: 2 times, first at 00:01:00 (1500), last at 00:03:00 (1500)" "${line}"

echo "guests-trim: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
