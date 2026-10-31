#!/bin/sh
# Keeps a test that outgrows the machine running it an ordinary failure, with what outgrew it in the
# log and the artifacts, rather than a runner that stops answering and keeps neither:
#
#   test/e2e/ci/guard.sh protect
#       the runner's own processes, its agent's ancestors and the services its connection needs, out
#       of the OOM killer's choice, and its agent's memory out of reclaim, so that pressure ends a
#       guest, which the test reports, and never the runner
#   test/e2e/ci/guard.sh watch <pid file> <record> [<vmstat record>] [<path>...]
#       every KUBEHENGE_E2E_GUARD_INTERVAL seconds (5) until stopped: the machine's memory, swap and
#       pressure, what of the swap zswap holds in memory and the free space of each path's file
#       system (/ by default) into <record>; once a
#       sample crosses a floor while the test that <pid file> names runs, its report into
#       <record>.fail, and the test ended
#   test/e2e/ci/guard.sh run <pid file> <record> <command>...
#       the command in a session of its own, whose ID it leaves in <pid file>, then the report of the
#       watch that ended it, if one did
#   test/e2e/ci/guard.sh peaks <record>
#       the least room and the most pressure the record holds, beside the floors
#   test/e2e/ci/guard.sh crossed <sample>
#       the floors one line of the record crosses, as watch judges them
#
# The floors leave the machine room to stop the test, collect from the guests and upload the
# artifacts, and lie well past what the tests take when they pass: their guests fit the machine's
# memory beside its own processes, but for the RKE2 test's control-plane replacements, whose third
# 4 GiB machine the swap takes about 2 GiB of, so that most of the swap's 11 GiB stays free. A
# record's swapped_in, the MiB read back from swap since the watch began, shows a test that leans on
# swap where it should not.
set -eu

here=$(dirname "$0")
proc=${KUBEHENGE_E2E_GUARD_PROC:-/proc}
interval=${KUBEHENGE_E2E_GUARD_INTERVAL:-5}

# MiB of memory and swap together. Below it the kernel is about to choose a process to end, and
# collecting from the guests needs some hundreds of MiB of its own.
room_floor=1536
# Percent of the last minute in which every task that could run waited on memory: the machine
# thrashing, too slow to answer for itself.
stall_ceiling=50
# MiB free on each watched file system, which the guests' disks, the logs and the artifacts fill.
disk_floor=2048
# Memory kept from reclaim for the runner's agent and the steps it runs, which share its cgroup: a
# few hundred MiB for the agent, the rest for the test's own tools.
runner_memory=$((1024 * 1048576))

# counter <name>: the kernel's count of that event since boot, from vmstat.
counter() {
    awk -v name="$1" '$1 == name { n = $2 } END { print n + 0 }' "${proc}/vmstat" 2> /dev/null || echo 0
}

# sample <OOM kills before the watch> <pages swapped in before it> <path>...: one line of the record:
# the time, then each value as name=value, in MiB and in percent of the last minute.
sample() {
    kills_before=$1 swapins_before=$2
    shift 2
    now=$(date -u +%T)
    line="${now} $(awk '/^MemAvailable:/ { a = $2 } /^SwapFree:/ { s = $2 }
        /^Zswap:/ { p = $2 } /^Zswapped:/ { z = $2; zswap = 1 }
        END { printf "available=%d swap_free=%d", a / 1024, s / 1024
            if (zswap) printf " zswapped=%d zswap_pool=%d", z / 1024, p / 1024 }' "${proc}/meminfo")"
    for kind in memory io; do
        [ -r "${proc}/pressure/${kind}" ] || continue
        line="${line} $(awk -v kind="${kind}" '{ for (i = 2; i <= NF; i++) if ($i ~ /^avg60=/) {
                sub(/^avg60=/, "", $i); printf "%s%s_%s=%s", sep, kind, $1, $i; sep = " " } }' \
            "${proc}/pressure/${kind}")"
    done
    kills=$(counter oom_kill)
    # Pages of 4 KiB, the size x86's kernel swaps.
    swapins=$(counter pswpin)
    line="${line} oom_kills=$((kills - kills_before)) swapped_in=$(((swapins - swapins_before) / 256))"
    for path in "$@"; do
        free=$(df -Pk "${path}" 2> /dev/null | awk 'NR == 2 { print int($4 / 1024) }')
        [ -z "${free}" ] || line="${line} free:${path}=${free}"
    done
    printf '%s\n' "${line}"
}

# crossed <sample>: each floor the sample crosses, separated by "; ", or nothing.
crossed() {
    printf '%s\n' "$1" | awk -v room="${room_floor}" -v stall="${stall_ceiling}" -v disk="${disk_floor}" '
        { for (i = 2; i <= NF; i++) { eq = index($i, "="); v[substr($i, 1, eq - 1)] = substr($i, eq + 1) } }
        END {
            out = ""
            if (("available" in v) && ("swap_free" in v) && v["available"] + v["swap_free"] < room)
                out = out sprintf("; %d MiB of memory and swap left, under %d", v["available"] + v["swap_free"], room)
            if (("memory_full" in v) && v["memory_full"] + 0 >= stall)
                out = out sprintf("; every task waited on memory %s%% of the last minute, %d%% at most", v["memory_full"], stall)
            if (v["oom_kills"] + 0 > 0)
                out = out sprintf("; the OOM killer ended %d process%s", v["oom_kills"], v["oom_kills"] > 1 ? "es" : "")
            for (k in v)
                if (k ~ /^free:/ && v[k] + 0 < disk)
                    out = out sprintf("; %d MiB free on %s, under %d", v[k], substr(k, 6), disk)
            if (out != "") print substr(out, 3)
        }'
}

# report <why> <sample> <vmstat record>: what holds the memory when a floor is crossed: the guests by
# name, the largest other processes, the last minute of vmstat, the pressure, and what the kernel
# said of memory.
report() {
    printf 'FAIL: the runner is running out of room, so the test was ended while it can still report: %s\n' "$1"
    printf 'the sample: %s\n' "$2"
    echo "the guests, most memory first: VM ID, name, resident MiB, swapped MiB"
    sh "${here}/guests.sh" now 2> /dev/null |
        awk '{ printf "%d   %s %s %d %d\n", ($6 + $7) / 1024, $3, $4, $6 / 1024, $7 / 1024 }' |
        sort -nr | sed 's/^[0-9]* //' || true
    echo "the largest other processes: PID, resident MiB, command"
    ps -eo pid=,rss=,comm= 2> /dev/null | awk '$3 != "kvm" { printf "%d   %s %d %s\n", $2, $1, $2 / 1024, $3 }' |
        sort -nr | head -n 12 | sed 's/^[0-9]* //' || true
    if [ -s "$3" ]; then
        echo "vmstat's last samples"
        sed -n '2p' "$3"
        tail -n 4 "$3"
    fi
    echo "pressure"
    for kind in memory io cpu; do
        [ ! -r "${proc}/pressure/${kind}" ] || sed "s/^/  ${kind} /" "${proc}/pressure/${kind}"
    done
    echo "the kernel on memory"
    sudo -n dmesg 2> /dev/null | grep -iE 'oom|out of memory|killed process|hung task' | tail -n 20 || true
}

# stop <process group>: asks the test to end, so that its own traps run, then ends what is left of it.
stop() {
    kill -TERM "-$1" 2> /dev/null || true
    waited=0
    while kill -0 "-$1" 2> /dev/null && [ "${waited}" -lt 20 ]; do
        sleep 1
        waited=$((waited + 1))
    done
    kill -KILL "-$1" 2> /dev/null || true
}

watch() {
    pidfile=$1 record=$2 vmstat=${3:-}
    shift 2
    [ "$#" -eq 0 ] || shift
    [ "$#" -gt 0 ] || set -- /
    kills_at_start=$(counter oom_kill)
    swapins_at_start=$(counter pswpin)
    fired=''
    while :; do
        line=$(sample "${kills_at_start}" "${swapins_at_start}" "$@")
        printf '%s\n' "${line}" >> "${record}"
        group=$(cat "${pidfile}" 2> /dev/null || true)
        # Only while the test runs: before it there is nothing to end, and after it the floors are
        # Collect's to cross.
        if [ -z "${fired}" ] && [ -n "${group}" ] && kill -0 "${group}" 2> /dev/null; then
            why=$(crossed "${line}")
            if [ -n "${why}" ]; then
                fired=1
                # shellcheck disable=SC2310 # the test is ended whatever the report could gather.
                report "${why}" "${line}" "${vmstat}" > "${record}.fail.part" 2>&1 || true
                mv "${record}.fail.part" "${record}.fail"
                stop "${group}"
            fi
        fi
        sleep "${interval}"
    done
}

run() {
    pidfile=$1 record=$2
    shift 2
    rm -f "${pidfile}" "${record}.fail"
    # The test's processes are the OOM killer's to choose like any other's: the runner's protection,
    # which they would inherit from its agent, is not theirs.
    { echo 0 > "/proc/$$/oom_score_adj"; } 2> /dev/null || true
    code=0
    # shellcheck disable=SC2016 # the session's own shell expands them.
    setsid --wait sh -c 'echo "$$" > "$0" && exec "$@"' "${pidfile}" "$@" || code=$?
    if [ -s "${record}.fail" ]; then
        why=$(sed -n '1s/^FAIL: //p' "${record}.fail")
        printf '::error title=The runner ran out of room::%s\n' "${why}"
        cat "${record}.fail"
        [ "${code}" -ne 0 ] || code=1
    fi
    return "${code}"
}

peaks() {
    [ -s "$1" ] || {
        echo "the guard recorded nothing"
        return 0
    }
    awk -v room="${room_floor}" -v stall="${stall_ceiling}" -v disk="${disk_floor}" '
        {
            delete v
            for (i = 2; i <= NF; i++) { eq = index($i, "="); v[substr($i, 1, eq - 1)] = substr($i, eq + 1) }
            left = v["available"] + v["swap_free"]
            if (n == 0 || left < least) { least = left; leastat = $1; available = v["available"]; swap = v["swap_free"] }
            if (n == 0 || v["available"] + 0 < lowmem) { lowmem = v["available"] + 0; lowmemat = $1 }
            for (k in v) {
                if (k ~ /_(some|full)$/ && (!(k in most) || v[k] + 0 > most[k])) { most[k] = v[k] + 0; mostat[k] = $1 }
                if (k ~ /^free:/ && (!(k in low) || v[k] + 0 < low[k])) { low[k] = v[k] + 0; lowat[k] = $1 }
            }
            kills = v["oom_kills"] + 0
            swapped = v["swapped_in"] + 0
            if (("zswapped" in v) && (zswappedat == "" || v["zswapped"] + 0 > zswapped)) { zswapped = v["zswapped"] + 0; pool = v["zswap_pool"] + 0; zswappedat = $1 }
            n++
        }
        END {
            printf "%d samples from %s to %s\n", n, first, $1
            printf "least memory and swap left: %d MiB (%d available, %d of swap) at %s; the floor is %d\n", least, available, swap, leastat, room
            printf "least memory available: %d MiB at %s\n", lowmem, lowmemat
            printf "read back from swap: %d MiB\n", swapped
            if (zswappedat != "") printf "most swap held in memory by zswap: %d MiB, compressed to %d MiB, at %s\n", zswapped, pool, zswappedat
            for (k in most) printf "most %s pressure: %.2f%% of a minute at %s%s\n", k, most[k], mostat[k], (k == "memory_full" ? sprintf("; the ceiling is %d%%", stall) : "")
            for (k in low) printf "least free on %s: %d MiB at %s; the floor is %d\n", substr(k, 6), low[k], lowat[k], disk
            printf "OOM kills: %d\n", kills
        }
        NR == 1 { first = $1 }' "$1"
}

protect() {
    # Whoever started this, up to init: the step's shell, the agent's worker and listener and what
    # started them; and the agent by name, with the services that keep it connected.
    pids=''
    pid=${PPID}
    while [ "${pid:-1}" -gt 1 ]; do
        pids="${pids} ${pid}"
        pid=$(awk '$1 == "PPid:" { print $2 }' "/proc/${pid}/status" 2> /dev/null || true)
    done
    pids="${pids} $(pgrep -x 'Runner\.(Listener|Worker)|systemd-(resolve|network)' | tr '\n' ' ')"
    echo "protected from the OOM killer: PID, oom_score_adj, resident MiB, command"
    # shellcheck disable=SC2086 # one PID each.
    list=$(printf '%s\n' ${pids} | sort -un)
    for pid in ${list}; do
        sudo choom -n -1000 -p "${pid}" > /dev/null || echo "the OOM killer may still choose ${pid}"
        adj=$(cat "/proc/${pid}/oom_score_adj" 2> /dev/null || echo gone)
        awk -v pid="${pid}" -v adj="${adj}" '$1 == "Name:" { name = $2 } $1 == "VmRSS:" { rss = $2 }
            END { printf "  %s %s %d %s\n", pid, adj, rss / 1024, name }' "/proc/${pid}/status" 2> /dev/null || true
    done
    # A cgroup's protection from reclaim holds only as far as each of its ancestors has as much.
    for agent in $(pgrep -x 'Runner\.(Listener|Worker)' || true); do
        sed -n 's/^0:://p' "/proc/${agent}/cgroup" 2> /dev/null || true
    done | sort -u | while read -r cgroup; do
        dir=/sys/fs/cgroup
        for part in $(printf '%s' "${cgroup}" | tr / ' '); do
            dir=${dir}/${part}
            [ -f "${dir}/memory.min" ] || continue
            have=$(cat "${dir}/memory.min")
            if [ "${have}" != max ] && [ "${have}" -lt "${runner_memory}" ]; then
                echo "${runner_memory}" | sudo tee "${dir}/memory.min" > /dev/null ||
                    echo "${dir} could not be kept from reclaim"
            fi
            have=$(cat "${dir}/memory.min")
            [ "${have}" = max ] || have="$((have / 1048576)) MiB"
            echo "kept from reclaim: ${have} of ${dir#/sys/fs/cgroup}"
        done
    done
}

usage="usage: $0 protect | watch <pid file> <record> [<vmstat record>] [<path>...] | run <pid file> <record> <command>... | peaks <record> | crossed <sample>"
command=${1:-}
[ "$#" -eq 0 ] || shift
case ${command} in
    protect) protect ;;
    watch)
        [ "$#" -ge 2 ] || {
            echo "${usage}" >&2
            exit 2
        }
        watch "$@"
        ;;
    run)
        [ "$#" -ge 3 ] || {
            echo "${usage}" >&2
            exit 2
        }
        run "$@"
        ;;
    peaks) peaks "${1:?${usage}}" ;;
    crossed) crossed "${1:?${usage}}" ;;
    *)
        echo "${usage}" >&2
        exit 2
        ;;
esac
