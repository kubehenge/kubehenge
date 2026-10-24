#!/bin/sh
# What the VMs of the Proxmox VE that test/e2e/ci/up.sh started take of the machine running it, and
# what their guests hold when a test fails:
#
#   test/e2e/ci/guests.sh record <file>    every 15 s until stopped: each VM's CPU time, memory and swap
#   test/e2e/ci/guests.sh summary <file>   from that record, each minute's CPUs and memory by VM, and
#                                          each VM's peaks: what a test's steps ask of the machine
#   test/e2e/ci/guests.sh collect <dir>    from inside each running VM, through its guest agent: its
#                                          state, the distribution's, the kernel's and cloud-init's
#                                          logs, the logs of the control plane's static pods and of
#                                          restarted containers, and its etcd members' view of their
#                                          cluster
#
# record and summary read the VMs' QEMU processes on this machine, which runs Proxmox VE's container
# on its own kernel; collect runs the commands in the container. Everything collect writes passes
# through redact, and each part is cut at a few megabytes.
set -eu

# A VM's QEMU process names its VM ID and name on its command line.
qemu_arg() { # pid option
    tr '\0' '\n' < "/proc/$1/cmdline" | sed -n "/^$2\$/{n;s/,.*//;p;q;}"
}

record() {
    while :; do
        now=$(date -u +%T)
        for pid in $(pgrep -x kvm || true); do
            # A VM that stops between the list and the read is left out of this sample.
            vmid=$(qemu_arg "${pid}" -id 2> /dev/null)
            name=$(qemu_arg "${pid}" -name 2> /dev/null)
            if [ -z "${vmid}" ] || [ -z "${name}" ]; then
                continue
            fi
            ticks=$(awk '{ print $14 + $15 }' "/proc/${pid}/stat" 2> /dev/null) || continue
            memory=$(awk '/^VmRSS:/ { r = $2 } /^VmSwap:/ { s = $2 } END { print r + 0, s + 0 }' \
                "/proc/${pid}/status" 2> /dev/null) || continue
            printf '%s %s %s %s %s %s\n' "${now}" "${pid}" "${vmid}" "${name}" "${ticks}" "${memory}"
        done
        sleep 15
    done
}

summary() {
    [ -s "$1" ] || {
        echo "no VM was sampled"
        return 0
    }
    hz=$(getconf CLK_TCK)
    awk -v hz="${hz}" '
        function seconds(t, p) { split(t, p, ":"); return p[1] * 3600 + p[2] * 60 + p[3] }
        {
            t = seconds($1); pid = $2; vm = $3; minute = substr($1, 1, 5)
            names[vm] = $4
            if (!(minute in seen)) { seen[minute] = 1; minutes[++n] = minute }
            mib = int(($6 + $7) / 1024); swap = int($7 / 1024)
            if (mib > top[minute, vm]) { top[minute, vm] = mib; topswap[minute, vm] = swap }
            if (mib > peak[vm]) { peak[vm] = mib; peakswap[vm] = swap; peakat[vm] = $1 }
            vms[minute, vm] = 1
            # CPU time over the time since the same process was last sampled.
            if ((pid in last) && t > lastt[pid]) {
                cpu[minute, vm] += ($5 - last[pid]) / hz
                span[minute, vm] += t - lastt[pid]
            }
            last[pid] = $5; lastt[pid] = t
        }
        END {
            print "VMs:"
            for (vm in names) printf "  %s %s\n", vm, names[vm]
            print "each minute: CPUs and resident MiB (swapped MiB) of all VMs, then of each VM"
            for (i = 1; i <= n; i++) {
                m = minutes[i]; line = ""; cpus = 0; mem = 0; swapped = 0
                for (vm in names) {
                    if (!((m, vm) in vms)) continue
                    c = span[m, vm] > 0 ? cpu[m, vm] / span[m, vm] : 0
                    if (c > cpupeak[vm]) { cpupeak[vm] = c; cpupeakat[vm] = m }
                    cpus += c; mem += top[m, vm] - topswap[m, vm]; swapped += topswap[m, vm]
                    line = line sprintf("  %s %.1f %d (%d)", vm, c, top[m, vm] - topswap[m, vm], topswap[m, vm])
                }
                printf "%s  %.1f %d (%d) |%s\n", m, cpus, mem, swapped, line
            }
            print "each VM: its busiest minute, and its most memory, resident and swapped together"
            for (vm in names)
                printf "  %s: %.1f CPUs at %s; %d MiB (%d swapped) at %s\n", vm, cpupeak[vm], cpupeakat[vm], peak[vm], peakswap[vm], peakat[vm]
        }' "$1"
}

# Tokens the distributions and their components might print: k3s's and RKE2's join tokens, and any
# value given as a token, password, secret or bearer.
redact() {
    sed -E -e 's/(K10[0-9a-f]+::[a-z]+:)[^[:space:]",]+/\1<redacted>/g' \
        -e 's/((token|password|secret)"?[[:space:]]*[=:][[:space:]]*"?)[^[:space:]",}]{8,}/\1<redacted>/gI' \
        -e 's/(bearer[[:space:]]+)[^[:space:]",}]{8,}/\1<redacted>/gI'
}

# guest_script <part>: the commands that print one part of a VM's state, run there by sh as root.
# shellcheck disable=SC2016 # the guest's shell expands them.
guest_script() {
    case $1 in
        state)
            echo 'uptime; echo; free -m; echo; grep -H . /proc/pressure/*; echo
                ps -eo pid,stat,pcpu,rss,etime,comm --sort=-rss | head -n 30; echo
                ps -eo label,pid,comm | grep -v kernel_t | head -n 150; echo
                systemctl --failed --no-pager --plain
                for u in k3s k3s-agent rke2-server rke2-agent kubelet containerd; do
                    systemctl cat "$u" > /dev/null 2>&1 || continue
                    echo; systemctl show -p Id,ActiveState,SubState,Result,NRestarts,ExecMainStatus,ExecMainStartTimestamp "$u"
                done'
            ;;
        # The distribution's own services, as many lines as are left of the last hour or so.
        journal)
            echo 'journalctl --no-pager -o short-iso-precise -n 20000 -u k3s -u k3s-agent -u rke2-server \
                -u rke2-agent -u kubelet -u containerd'
            ;;
        # Every line about etcd of the server's whole journal, which would otherwise have rolled out
        # of the part above when the API stayed down for long; RKE2's and kubeadm's etcd run as static
        # pods, whose logs are below. The API server's watches report etcd's errors again, one line
        # per watch each time, so that a minute without a leader would leave only them.
        etcd-journal)
            echo 'journalctl --no-pager -o short-iso-precise -u k3s -u rke2-server |
                grep -iE "etcd|raft|learner|promot|quorum|leader|member" |
                grep -vE "watcher\.go|reflector\.go|cacher\.go" | tail -n 3000'
            ;;
        # The control plane's static pods and kube-vip as the kubelet keeps their logs on the node,
        # readable while the API is down; the previous log of every container that restarted beside
        # them; RKE2's kubelet and the containerd of RKE2 and k3s, which log to files.
        logs)
            echo 'for f in /var/log/pods/kube-system_etcd-*/*/*.log /var/log/pods/kube-system_kube-apiserver-*/*/*.log \
                    /var/log/pods/kube-system_kube-vip-*/*/*.log; do
                    [ -f "$f" ] && { echo "== $f"; tail -n 1500 "$f"; }
                done
                for d in /var/log/pods/*/*/; do
                    [ "$(ls "$d" | wc -l)" -gt 1 ] || continue
                    f=$d$(ls "$d" | sort -n | head -n 1)
                    echo "== $f (an earlier run of the container)"; tail -n 400 "$f"
                done
                for f in /var/lib/rancher/*/agent/logs/kubelet.log /var/lib/rancher/*/agent/containerd/containerd.log; do
                    [ -f "$f" ] && { echo "== $f"; tail -n 2000 "$f"; }
                done'
            ;;
        kernel)
            echo 'dmesg -T 2>&1 | tail -n 1500'
            ;;
        # The bootstrap's commands as cloud-init ran them, with what each printed, and cloud-init's
        # own warnings and errors.
        cloud-init)
            echo 'tail -n 3000 /var/log/cloud-init-output.log; echo
                grep -E "WARNING|ERROR|Traceback" /var/log/cloud-init.log | tail -n 300'
            ;;
        # The members of this node's etcd as it sees them, its own status (leader, raft index,
        # learner, errors) and health, through etcd's gateway with the node's etcd client
        # certificate, and the metrics that tell a lost leader from a slow disk. k3s and RKE2 serve
        # the gateway on a port of its own, kubeadm's etcd on its client port; all serve metrics on
        # 2381. Nothing on a node without a certificate.
        etcd)
            echo 'set --
                for d in /var/lib/rancher/k3s/server/tls/etcd /var/lib/rancher/rke2/server/tls/etcd; do
                    [ -f "$d/client.crt" ] && set -- --cacert "$d/server-ca.crt" --cert "$d/client.crt" --key "$d/client.key" && port=2382
                done
                p=/etc/kubernetes/pki/etcd
                [ -f "$p/healthcheck-client.crt" ] && port=2379 &&
                    set -- --cacert "$p/ca.crt" --cert "$p/healthcheck-client.crt" --key "$p/healthcheck-client.key"
                [ "$#" -gt 0 ] || { echo "no etcd client certificate on this node"; exit 0; }
                for path in cluster/member/list maintenance/status; do
                    echo "== $path"; curl -sS --max-time 10 "$@" -X POST -d "{}" "https://127.0.0.1:$port/v3/$path"; echo
                done
                echo "== health"; curl -sS --max-time 10 "$@" "https://127.0.0.1:$port/health"; echo
                echo "== metrics"; curl -sS --max-time 10 http://127.0.0.1:2381/metrics |
                    grep -E "^etcd_(server_(has_leader|is_leader|is_learner|leader_changes_seen_total|heartbeat_send_failures_total|proposals_failed_total|slow_apply_total|learner_promote_)|disk_wal_fsync_duration_seconds_(count|sum)|network_peer_round_trip_time_seconds_(count|sum))"'
            ;;
        *) return 1 ;;
    esac
}

# in_guest <vmid> <commands>: runs the commands in the VM through its guest agent and prints their
# output, error output and exit code, or why they could not run.
in_guest() {
    if ! result=$(docker exec kubehenge-e2e-pve qm guest exec "$1" --timeout 120 -- sh -c "$2" 2>&1); then
        printf '%s\n' "${result}"
        return 1
    fi
    # The truncation flags come beside any output, false as 0, which jq would take for true.
    printf '%s' "${result}" | jq -r '((."out-data", ."err-data") // empty | select(. != "")),
        (if [."out-truncated", ."err-truncated"] | any(. == true or . == 1)
            then "(the guest agent cut the output short)" else empty end),
        "exit code: \(.exitcode // "none, still running when the wait ended")"'
}

collect() {
    out=$1
    mkdir -p "${out}"
    docker exec kubehenge-e2e-pve qm list | awk 'NR > 1 && $3 == "running" { print $1, $2 }' |
        while read -r vmid name; do
            dir="${out}/${vmid}-${name}" && mkdir -p "${dir}"
            # A VM whose agent does not answer, or that has no shell, as Talos has none, is tried
            # once; its first part says why.
            commands=$(guest_script state)
            in_guest "${vmid}" "${commands}" 2>&1 | redact | head -c 1000000 > "${dir}/state.txt"
            grep -q '^exit code: 0$' "${dir}/state.txt" || continue
            for part in journal etcd-journal logs kernel cloud-init etcd; do
                commands=$(guest_script "${part}")
                in_guest "${vmid}" "${commands}" 2>&1 | redact | head -c 8000000 > "${dir}/${part}.txt"
            done
        done
}

case ${1:-} in
    record) record > "${2:?usage: $0 record <file>}" ;;
    summary) summary "${2:?usage: $0 summary <file>}" ;;
    collect) collect "${2:?usage: $0 collect <directory>}" ;;
    *)
        echo "usage: $0 record <file> | summary <file> | collect <directory>" >&2
        exit 2
        ;;
esac
