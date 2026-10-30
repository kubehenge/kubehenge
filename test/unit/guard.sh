#!/bin/sh
# Unit test of test/e2e/ci/guard.sh: which samples cross its floors, what a sample reads from the
# kernel's files and df, the peaks of a record, and a watch that ends a test, against a /proc of its
# own and stand-ins for df, ps, pgrep and sudo. Ending a test needs setsid, so that part is skipped
# where there is none.
#
#   test/unit/guard.sh
set -eu

here=$(dirname "$0")
guard=${here}/../e2e/ci/guard.sh
root=$(mktemp -d)
bin=${root}/bin proc=${root}/proc
mkdir "${bin}" "${proc}" "${proc}/pressure"
watcher=''
finish() {
    [ -z "${watcher}" ] || kill "${watcher}" 2> /dev/null || true
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
# truth <what> <command>...: whether the command succeeds.
truth() {
    what=$1
    shift
    if "$@"; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n' "${what}"
        failures=$((failures + 1))
    fi
}
# holds <what> <text wanted> <text>: whether the text holds what is wanted.
holds() {
    case "$3" in
        *"$2"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted it to hold: %s\n  got: %s\n' "$1" "$2" "$3"
            failures=$((failures + 1))
            ;;
    esac
}

cat > "${bin}/df" << 'EOF'
#!/bin/sh
echo 'Filesystem 1024-blocks Used Available Capacity Mounted on'
case $2 in
    */full) echo "/dev/full 1048576 1047552 1024 99% $2" ;;
    *) echo "/dev/root 152043520 72351744 79691776 48% $2" ;;
esac
EOF
cat > "${bin}/ps" << 'EOF'
#!/bin/sh
printf '%s\n' '  77 4194304 kvm' ' 1234 2097152 Runner.Worker' '  55 524288 dockerd'
EOF
cat > "${bin}/pgrep" << 'EOF'
#!/bin/sh
exit 1
EOF
cat > "${bin}/sudo" << 'EOF'
#!/bin/sh
[ "$*" = '-n dmesg' ] && echo '[3600.1] Out of memory: Killed process 77 (kvm)'
EOF
chmod +x "${bin}"/*
export PATH="${bin}:${PATH}" KUBEHENGE_E2E_GUARD_PROC="${proc}" KUBEHENGE_E2E_GUARD_INTERVAL=1

# memory <available kB> <swap free kB> <memory full avg60> <OOM kills> [<pages swapped in>]: the
# kernel's files as the guard reads them.
memory() {
    printf 'MemTotal: 16366592 kB\nMemFree: 300000 kB\nMemAvailable: %s kB\nSwapTotal: 11533312 kB\nSwapFree: %s kB\n' \
        "$1" "$2" > "${proc}/meminfo"
    printf 'some avg10=12.50 avg60=8.25 avg300=2.00 total=1\nfull avg10=4.00 avg60=%s avg300=1.00 total=1\n' \
        "$3" > "${proc}/pressure/memory"
    printf 'some avg10=30.00 avg60=20.75 avg300=9.00 total=1\nfull avg10=10.00 avg60=6.50 avg300=3.00 total=1\n' \
        > "${proc}/pressure/io"
    printf 'pgfault 1\npswpin %s\npswpout 9\noom_kill %s\npgmajfault 2\n' "${5:-1000}" "$4" > "${proc}/vmstat"
}

crossed() { sh "${guard}" crossed "$1"; }
# The deepest a passing run went: rke2's at a rolling upgrade, 4.9 GiB of its guests swapped.
calm="12:00:00 available=300 swap_free=5200 memory_some=30.00 memory_full=12.00 io_some=40.00 io_full=20.00 oom_kills=0 free:/=70000 free:/mnt=60000"
expect "a peak of a passing run crosses nothing" "" crossed "${calm}"
expect "memory and swap at the floor are still enough" "" crossed "12:00:00 available=36 swap_free=1500 oom_kills=0"
expect "memory and swap under the floor" "1535 MiB of memory and swap left, under 1536" \
    crossed "12:00:00 available=35 swap_free=1500 oom_kills=0"
expect "every task waiting on memory half the minute" "every task waited on memory 50.00% of the last minute, 50% at most" \
    crossed "12:00:00 available=300 swap_free=6000 memory_full=50.00 oom_kills=0"
expect "just under the stall ceiling" "" crossed "12:00:00 available=300 swap_free=6000 memory_full=49.99 oom_kills=0"
expect "an OOM kill" "the OOM killer ended 1 process" crossed "12:00:00 available=300 swap_free=6000 oom_kills=1"
expect "a file system nearly full" "2047 MiB free on /mnt, under 2048" \
    crossed "12:00:00 available=300 swap_free=6000 oom_kills=0 free:/=70000 free:/mnt=2047"
expect "two floors at once" "1000 MiB of memory and swap left, under 1536; the OOM killer ended 2 processes" \
    crossed "12:00:00 available=100 swap_free=900 oom_kills=2"
expect "a kernel without pressure files" "" crossed "12:00:00 available=300 swap_free=6000 oom_kills=0 free:/=70000"

# A watch with no test running records and never ends anything, whatever the floors say.
memory 102400 409600 75.00 3
record=${root}/guard.txt
sh "${guard}" watch "${root}/test.pid" "${record}" "${root}/vmstat.txt" / "${root}/full" > "${root}/watch.log" 2>&1 &
watcher=$!
# sampled <text>: waits up to 20 seconds for a sample holding the text. A watch that starts slowly
# would otherwise take its first sample after the swap-in below.
sampled() {
    for _ in $(seq 100); do
        if grep -q -F -- "$1" "${record}" 2> /dev/null; then return 0; fi
        sleep 0.2
    done
    printf 'FAIL: no sample holds %s\n' "$1"
    failures=$((failures + 1))
}
sampled "swapped_in=0"
# 10 MiB read back from swap.
memory 102400 409600 75.00 3 3560
sampled "swapped_in=10"
kill "${watcher}"
watcher=''
line=$(head -n 1 "${record}")
holds "a sample reads the memory in MiB" "available=100 swap_free=400" "${line}"
holds "and the pressure over the last minute" "memory_some=8.25 memory_full=75.00 io_some=20.75 io_full=6.50" "${line}"
holds "and the OOM kills and swap-ins since the watch began" "oom_kills=0 swapped_in=0" "${line}"
holds "and each path's free space" "free:/=77824 free:${root}/full=1" "${line}"
last=$(tail -n 1 "${record}")
holds "swap-ins counted from the watch's start, in MiB" "swapped_in=10" "${last}"
truth "no test, nothing ended" [ ! -e "${record}.fail" ]

peaks=$(sh "${guard}" peaks "${record}")
holds "the peaks give the least room" "least memory and swap left: 500 MiB (100 available, 400 of swap)" "${peaks}"
holds "and the least memory alone" "least memory available: 100 MiB at " "${peaks}"
holds "and what was read back from swap" "read back from swap: 10 MiB" "${peaks}"
holds "and the most stall, beside its ceiling" "most memory_full pressure: 75.00% of a minute" "${peaks}"
holds "and the least free space" "least free on ${root}/full: 1 MiB" "${peaks}"
expect "nothing recorded" "the guard recorded nothing" sh "${guard}" peaks "${root}/none.txt"

if command -v setsid > /dev/null 2>&1; then
    rm -f "${record}"
    memory 4096000 6144000 2.00 3
    code=0
    out=$(sh "${guard}" run "${root}/test.pid" "${record}" sh -c 'exit 3') || code=$?
    truth "a test's own failure is its exit code" [ "${code}" = 3 ]
    truth "with no report" [ -z "${out}" ]

    sh "${guard}" watch "${root}/test.pid" "${record}" "${root}/vmstat.txt" / > "${root}/watch.log" 2>&1 &
    watcher=$!
    printf 'procs memory\n r b swpd free\n 1 0 0 300 12:00:00\n 9 4 900 100 12:00:15\n' > "${root}/vmstat.txt"
    # The kernel ends a process a moment into the test.
    (sleep 2 && memory 4096000 6144000 2.00 4) &
    start=$(date +%s)
    code=0
    out=$(sh "${guard}" run "${root}/test.pid" "${record}" sh -c 'trap "" TERM; sleep 60') || code=$?
    took=$(($(date +%s) - start))
    kill "${watcher}"
    watcher=''
    truth "an ended test fails" [ "${code}" -ne 0 ]
    truth "within the watch's wait for it to end" [ "${took}" -lt 40 ]
    holds "an annotation says why" "::error title=The runner ran out of room::the runner is running out of room, so the test was ended while it can still report: the OOM killer ended 1 process" "${out}"
    holds "the report gives the sample" "the sample: " "${out}"
    holds "the largest processes but the guests" "1234 2048 Runner.Worker" "${out}"
    holds "vmstat's last samples" " 9 4 900 100 12:00:15" "${out}"
    holds "the pressure" "memory full avg10=4.00 avg60=2.00" "${out}"
    holds "what the kernel said" "Out of memory: Killed process 77 (kvm)" "${out}"
else
    echo "guard: no setsid here, so ending a test is left to Linux"
fi

echo "guard: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
