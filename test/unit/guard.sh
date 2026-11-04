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
# kernel's files as the guard reads them, with 30 MiB of swap that zswap holds in 10 MiB.
memory() {
    printf 'MemTotal: 16366592 kB\nMemFree: 300000 kB\nMemAvailable: %s kB\nCached: 1126400 kB\nSwapTotal: 11533312 kB\nSwapFree: %s kB\nZswap: 10240 kB\nZswapped: 30720 kB\nShmem: 102400 kB\n' \
        "$1" "$2" > "${proc}/meminfo"
    printf 'some avg10=12.50 avg60=8.25 avg300=2.00 total=1\nfull avg10=4.00 avg60=%s avg300=1.00 total=1\n' \
        "$3" > "${proc}/pressure/memory"
    printf 'some avg10=30.00 avg60=20.75 avg300=9.00 total=1\nfull avg10=10.00 avg60=6.50 avg300=3.00 total=1\n' \
        > "${proc}/pressure/io"
    printf 'some avg10=97.00 avg60=81.25 avg300=40.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n' \
        > "${proc}/pressure/cpu"
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
holds "a sample reads the memory in MiB, with the page cache but tmpfs" "available=100 cache=1000 swap_free=400" "${line}"
holds "and the pressure over the last 10 seconds and the last minute" \
    "memory_some10=12.50 memory_some=8.25 memory_full10=4.00 memory_full=75.00 io_some10=30.00 io_some=20.75 io_full10=10.00 io_full=6.50" "${line}"
holds "the CPUs' pressure too" "cpu_some10=97.00 cpu_some=81.25" "${line}"
expect "a 10 seconds' stall is no minute's" "" crossed "12:00:00 available=300 swap_free=6000 memory_full10=90.00 memory_full=12.00 oom_kills=0"
holds "and the OOM kills and swap-ins since the watch began" "oom_kills=0 swapped_in=0" "${line}"
holds "and what zswap holds, in MiB" "zswapped=30 zswap_pool=10" "${line}"
holds "and each path's free space" "free:/=77824 free:${root}/full=1" "${line}"
last=$(tail -n 1 "${record}")
holds "swap-ins counted from the watch's start, in MiB" "swapped_in=10" "${last}"
truth "no test, nothing ended" [ ! -e "${record}.fail" ]

peaks=$(sh "${guard}" peaks "${record}")
holds "the peaks give the least room" "least memory and swap left: 500 MiB (100 available, 400 of swap)" "${peaks}"
holds "and the least memory alone, beside the floor without swap" "least memory available: 100 MiB at " "${peaks}"
holds "and the least page cache" "least page cache: 1000 MiB at " "${peaks}"
holds "and the most stall of 10 seconds" "most cpu_some pressure: 97.00% of 10 seconds at " "${peaks}"
holds "and what was read back from swap" "read back from swap: 10 MiB" "${peaks}"
holds "and the most zswap held" "most swap held in memory by zswap: 30 MiB, compressed to 10 MiB, at " "${peaks}"
holds "and the most stall, beside its ceiling" "most memory_full pressure: 75.00% of a minute" "${peaks}"
holds "and the least free space" "least free on ${root}/full: 1 MiB" "${peaks}"
expect "nothing recorded" "the guard recorded nothing" sh "${guard}" peaks "${root}/none.txt"
truth "a watch without a talosconfig reads no management node" [ ! -e "${record}.management" ]

# The management node's memory, through a stand-in talosctl that answers while ${root}/talos-up is
# there and leaves the arguments it was given in ${root}/talosctl.args.
cat > "${bin}/talosctl" << EOF
#!/bin/sh
echo "\$*" >> "${root}/talosctl.args"
[ -e "${root}/talos-up" ] || exit 1
case "\$*" in
    *' read /proc/meminfo') printf 'MemTotal: 3991552 kB\nMemFree: 300000 kB\nMemAvailable: 1556480 kB\nCached: 1200000 kB\n' ;;
    *' read /proc/pressure/memory') printf 'some avg10=1.50 avg60=0.75 avg300=0.10 total=5\nfull avg10=0.50 avg60=60.00 avg300=0.05 total=2\n' ;;
    *) exit 1 ;;
esac
EOF
chmod +x "${bin}/talosctl"
memory 4096000 6144000 2.00 3
rm -f "${record}"
talosconfig=${root}/talosconfig
KUBEHENGE_E2E_GUARD_TALOSCONFIG=${talosconfig} sh "${guard}" watch "${root}/test.pid" "${record}" > "${root}/watch.log" 2>&1 &
watcher=$!
sampled "swapped_in=0"
truth "no talosconfig yet, no node asked" [ ! -e "${root}/talosctl.args" ]
printf 'context: e2e\ncontexts:\n    e2e:\n        endpoints:\n            - 172.20.99.11\n            - 172.20.99.12\n        nodes:\n            - 172.20.99.11\n            - 172.20.99.12\n        ca: x\n' > "${talosconfig}"
touch "${root}/talos-up"
sampled "mgmt_used="
# A node that stops answering leaves no figures behind.
rm -f "${root}/talos-up"
gone=''
for _ in $(seq 100); do
    case "$(tail -n 1 "${record}")" in
        *mgmt_*) sleep 0.2 ;;
        *)
            gone=1
            break
            ;;
    esac
done
kill "${watcher}"
watcher=''
truth "a node that stops answering leaves no figures in the samples after it" [ -n "${gone}" ]
first=$(head -n 1 "${record}")
case "${first}" in
    *mgmt_*) bad=1 ;;
    *) bad='' ;;
esac
truth "a sample before the talosconfig holds no figures of the node" [ -z "${bad}" ]
mgmt=$(grep -m 1 mgmt_ "${record}")
holds "the node's memory used and available, in MiB" "mgmt_used=2378 mgmt_available=1520" "${mgmt}"
holds "and its memory pressure over 10 seconds and the minute" \
    "mgmt_memory_some10=1.50 mgmt_memory_some=0.75 mgmt_memory_full10=0.50 mgmt_memory_full=60.00" "${mgmt}"
asked=$(cat "${root}/talosctl.args")
holds "read from the talosconfig's first node" "--talosconfig ${talosconfig} -n 172.20.99.11 read /proc/meminfo" "${asked}"
expect "the node's pressure ends no test, the machine's does" "" crossed "${mgmt}"
mgmt_peaks=$(sh "${guard}" peaks "${record}")
holds "the peaks give the node's most memory used" "the management node's most memory used: 2378 MiB at " "${mgmt_peaks}"
holds "and its least available" "the management node's least memory available: 1520 MiB at " "${mgmt_peaks}"
holds "and its pressure, named as its own" "most memory_full pressure on the management node: 60.00% of a minute at " "${mgmt_peaks}"
holds "with the machine's still beside its ceiling" "most memory_full pressure: 2.00% of a minute at " "${mgmt_peaks}"

# Memory without swap: a stretch under 1024 MiB counts from a minute on, whatever swap is left.
short() { sh "${guard}" short "$1"; }
printf '%s\n' '10:00:00 available=2000 swap_free=9000' '10:00:05 available=1023 swap_free=9000' \
    '10:00:35 available=600 swap_free=9000' '10:01:04 available=900 swap_free=9000' \
    '10:01:09 available=1024 swap_free=9000' > "${root}/brief.txt"
expect "under the floor for 59 s" "" short "${root}/brief.txt"
printf '%s\n' '23:59:00 available=2000 swap_free=9000' '23:59:30 available=500 swap_free=9000' \
    '00:00:30 available=300 swap_free=9000' '00:00:35 available=4000 swap_free=9000' \
    '00:10:00 available=800 swap_free=9000' '00:11:10 available=700 swap_free=9000' > "${root}/starved.txt"
expect "stretches of a minute and more, across midnight and to the record's end" \
    "130 s with under 1024 MiB of memory available, swap not counted, in 2 stretches, the longest 70 s from 00:10:00 to 00:11:10; 300 MiB at the least" \
    short "${root}/starved.txt"
starved_peaks=$(sh "${guard}" peaks "${root}/starved.txt")
holds "the peaks give them too" "130 s with under 1024 MiB" "${starved_peaks}"
expect "a record without them" "" short "${record}"
expect "no record" "" short "${root}/none.txt"

# Pressure and reads back from swap past their lines, over 10 seconds and over a minute.
strained() { sh "${guard}" strained "$1"; }
expect "a stretch under the floor without pressure strains nothing" "" strained "${root}/starved.txt"
printf '%s\n' '10:00:00 available=900 memory_full10=9.00 swapped_in=0' '10:00:30 available=800 memory_full10=10.00 swapped_in=200' \
    '10:01:05 available=700 memory_full10=2.00 swapped_in=450' > "${root}/leaning.txt"
expect "at the lines, or more in more than a minute, is no strain" "" strained "${root}/leaning.txt"
printf '%s\n' '23:59:40 available=900 memory_full10=1.00 swapped_in=100' '00:00:10 available=800 memory_full10=12.50 mgmt_memory_full10=90.00 swapped_in=200' \
    '00:00:40 available=700 memory_full10=3.00 swapped_in=400' > "${root}/strained.txt"
expect "pressure and a minute's swap reads past their lines, across midnight" \
    "every task waited on memory 12.50% of 10 seconds at 00:00:10, past the line of 10%; 300 MiB read back from swap in the minute to 00:00:40, past the line of 256 MiB" \
    strained "${root}/strained.txt"
printf '%s\n' '10:00:00 memory_full10=1.00 mgmt_memory_full10=95.00 swapped_in=0' > "${root}/node.txt"
expect "the management node's own pressure strains nothing" "" strained "${root}/node.txt"
strained_peaks=$(sh "${guard}" peaks "${root}/strained.txt")
holds "the peaks give the strain" "past the line of 10%" "${strained_peaks}"
holds "and the line beside the pressure" "most memory_full pressure: 12.50% of 10 seconds at 00:00:10; the line is 10%" "${strained_peaks}"
expect "no record strains nothing" "" strained "${root}/none.txt"

if command -v setsid > /dev/null 2>&1; then
    rm -f "${record}"
    memory 4096000 6144000 2.00 3
    code=0
    start=$(date +%s)
    out=$(sh "${guard}" run "${root}/test.pid" "${record}" sh -c 'exit 3') || code=$?
    took=$(($(date +%s) - start))
    truth "a test's own failure is its exit code" [ "${code}" = 3 ]
    truth "with no report" [ -z "${out}" ]
    truth "and the log's pipe closed as it ends" [ "${took}" -lt 10 ]

    echo "12:00:00 available=4000 swap_free=6000 oom_kills=0" > "${record}"
    out=$(KUBEHENGE_E2E_GUARD_REPORT=1 sh "${guard}" run "${root}/test.pid" "${record}" sleep 3)
    holds "a running test's log gets the latest sample" "runner: 12:00:00 available=4000 swap_free=6000 oom_kills=0" "${out}"
    rm -f "${record}"

    cp "${root}/starved.txt" "${record}"
    code=0
    out=$(sh "${guard}" run "${root}/test.pid" "${record}" sh -c 'exit 3') || code=$?
    truth "a failed test short of available memory keeps its exit code" [ "${code}" = 3 ]
    holds "with the stretch as a figure" "memory available: 130 s with under 1024 MiB" "${out}"
    case "${out}" in
        *::error* | *::warning*) annotated=1 ;;
        *) annotated='' ;;
    esac
    truth "and no annotation without pressure" [ -z "${annotated}" ]
    cp "${root}/strained.txt" "${record}"
    code=0
    out=$(sh "${guard}" run "${root}/test.pid" "${record}" sh -c 'exit 3') || code=$?
    truth "a failed test whose guests strained the machine keeps its exit code" [ "${code}" = 3 ]
    holds "and is named an error beside it" "::error title=The guests strained the runner's memory::every task waited on memory 12.50%" "${out}"
    code=0
    out=$(sh "${guard}" run "${root}/test.pid" "${record}" true) || code=$?
    truth "a passed one still passes" [ "${code}" = 0 ]
    holds "with a warning" "::warning title=The guests strained the runner's memory::every task waited on memory 12.50%" "${out}"
    rm -f "${record}"

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
