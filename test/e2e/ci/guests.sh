#!/bin/sh
# What the VMs of the Proxmox VE that test/e2e/ci/up.sh started take of the machine running it, and
# what their guests hold when a test fails:
#
#   test/e2e/ci/guests.sh record <file>    every 15 s until stopped: each VM's CPU time, memory and swap
#   test/e2e/ci/guests.sh summary <file>   from that record, each minute's CPUs and memory by VM, and
#                                          each VM's peaks: what a test's steps ask of the machine
#   test/e2e/ci/guests.sh collect <dir> [<work dir>]
#                                          from inside each running VM, through its guest agent: its
#                                          state, the distribution's, the kernel's and cloud-init's
#                                          logs, the logs of the control plane's static pods and of
#                                          restarted containers, and its etcd members' view of their
#                                          cluster; from a Talos VM, whose guest agent runs no
#                                          commands, the same through Talos's API
#
# record and summary read the VMs' QEMU processes on this machine, which runs Proxmox VE's container
# on its own kernel; collect runs the commands in the container. Everything collect writes passes
# through redact, and each part is cut at a few megabytes.
#
# collect reads a Talos VM from this machine with talosctl and the talosconfig of the VM's cluster:
# the management cluster's from the Terraform/OpenTofu state in run.sh's work directory (by default
# $RUNNER_TEMP/e2e, where the workflow keeps it), a workload cluster's from its <cluster>-talosconfig
# Secret, read with the management cluster's kubeconfig from the same place. They stay in a private
# directory that collect removes when it ends, never in what it writes. A VM runs Talos when it is
# tagged management (the management cluster's nodes) or talos, or when its cluster has a
# talosconfig. Each talosctl call is cut off after KUBEHENGE_E2E_TALOSCTL_TIMEOUT seconds (60), and
# a node whose API does not answer the first is asked nothing more.
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

talos_timeout=${KUBEHENGE_E2E_TALOSCTL_TIMEOUT:-60}

# talosctl_node <talosctl arguments...>: asks the node at ${talos_address} directly with
# ${talos_config}, rather than through a control plane, and leaves what talosctl printed in
# ${private}/talosctl.out and its exit code in talos_code, 124 or 137 when it was cut off.
talosctl_node() {
    talos_code=0
    timeout -k 10 "${talos_timeout}" talosctl --talosconfig "${talos_config}" --endpoints "${talos_address}" \
        --nodes "${talos_address}" "$@" < /dev/null > "${private}/talosctl.out" 2>&1 || talos_code=$?
}

# talos <lines> <talosctl arguments...>: the command, the last lines of what it printed and its exit
# code, as in_guest ends what a guest printed.
talos() {
    talos_lines=$1
    shift
    echo "== talosctl $*"
    talosctl_node "$@"
    tail -n "${talos_lines}" "${private}/talosctl.out"
    case ${talos_code} in
        124 | 137) echo "exit code: none, cut off after ${talos_timeout} s" ;;
        *) echo "exit code: ${talos_code}" ;;
    esac
}

# talos_pod_logs: the API server's logs as the kubelet keeps them on the node, readable while the
# API is down, and the previous log of every container that restarted, as guest_script's logs part
# reads them elsewhere. Talos runs etcd as a service, whose log is in the services part.
talos_pod_logs() {
    talos 100000 list /var/log/pods -r > "${private}/pod-list"
    if [ "${talos_code}" -ne 0 ]; then
        cat "${private}/pod-list"
        return 0
    fi
    # talosctl names each file below the directory it lists.
    awk '$NF ~ /\.log$/ && $1 != "==" { f = $NF; if (f !~ /^\//) f = "/var/log/pods/" f; print f }' \
        "${private}/pod-list" > "${private}/pod-logs"
    grep -E '^/var/log/pods/kube-system_kube-apiserver-[^/]+/[^/]+/[^/]+$' "${private}/pod-logs" |
        while read -r f; do talos 1500 read "${f}"; done
    # The kubelet numbers a container's runs, and keeps the one before the current.
    awk '{ d = $0; sub(/[^\/]+$/, "", d); n = substr($0, length(d) + 1) + 0; count[d]++
            if (!(d in low) || n < low[d]) { low[d] = n; first[d] = $0 } }
        END { for (d in count) if (count[d] > 1) print first[d] }' "${private}/pod-logs" |
        while read -r f; do
            echo "== an earlier run of the container"
            talos 400 read "${f}"
        done
}

# talos_part <part>: one part of a Talos node's state, as guest_script prints another VM's: the
# machine's stage, its services' states and events, its memory, pressure and processes and the
# kubelet's containers; its services' logs; the pods' logs; the kernel's log; etcd's members, status
# and alarms. ${talos_services} names the node's services as it listed them.
talos_part() {
    case $1 in
        state)
            cat "${private}/version" "${private}/services"
            talos 100 get machinestatus -o yaml
            for s in ${talos_services}; do talos 60 service "${s}"; done
            talos 100 memory
            for f in /proc/uptime /proc/loadavg /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io; do
                talos 10 read "${f}"
            done
            talos 1000 processes
            talos 1000 containers --kubernetes
            ;;
        services)
            for s in ${talos_services}; do talos 10000 logs "${s}" --tail 10000; done
            ;;
        logs) talos_pod_logs ;;
        kernel) talos 1500 dmesg ;;
        etcd)
            talos 200 etcd members
            talos 100 etcd status
            talos 100 etcd alarm list
            ;;
        *) return 1 ;;
    esac
}

# talos_collect <dir>: a Talos VM's parts, read through its API. A node whose API does not answer is
# asked once, and its state says why.
talos_collect() {
    {
        echo "Talos, read through its API at ${talos_address}"
        talos 20 version
    } > "${private}/version"
    if [ "${talos_code}" -ne 0 ]; then
        {
            cat "${private}/version"
            echo "its API does not answer, so nothing more was asked of it"
        } | redact | head -c 1000000 > "$1/state.txt"
        return 0
    fi
    talos 100 services > "${private}/services"
    talos_services=$(awk '$1 != "==" && $2 ~ /^(machined|apid|trustd|containerd|cri|kubelet|etcd)$/ { printf "%s ", $2 }' \
        "${private}/talosctl.out")
    [ -n "${talos_services}" ] || talos_services='machined apid trustd containerd cri kubelet etcd '
    for part in state services logs kernel etcd; do
        # etcd runs on the control planes only.
        if [ "${part}" = etcd ]; then
            case " ${talos_services}" in
                *" etcd "*) ;;
                *) continue ;;
            esac
        fi
        size=8000000
        [ "${part}" != state ] || size=1000000
        talos_part "${part}" 2>&1 | redact | head -c "${size}" > "$1/${part}.txt"
    done
}

# tofu_output <output> <file>: one of the management cluster's Terraform/OpenTofu outputs into the
# file, or why it could not be had into <file>.why.
tofu_output() {
    if [ -z "${work}" ] || [ ! -d "${work}/mgmt" ]; then
        echo "no Terraform/OpenTofu state in '${work}/mgmt'" > "$2.why"
        return 1
    fi
    tofu_code=0
    timeout -k 10 60 tofu -chdir="${work}/mgmt" output -raw "$1" < /dev/null > "$2" 2> "$2.why" || tofu_code=$?
    if [ "${tofu_code}" -ne 0 ]; then
        rm -f "$2"
        echo "Terraform/OpenTofu gave no $1, exit code ${tofu_code}" >> "$2.why"
        return 1
    fi
}

mgmt_kubectl() {
    timeout -k 10 60 kubectl --kubeconfig "${kubeconfig}" --request-timeout=20s "$@" < /dev/null
}

# talos_machines: each machine Cluster API made on Proxmox VE as "<VM ID> <namespace> <cluster>
# <address>" in ${private}/machines, from the management cluster; nothing, with why in
# ${private}/machines.why, when it does not answer. Clears kubeconfig then.
talos_machines() {
    : > "${private}/machines"
    kubeconfig=${work:+${work}/kubeconfig.yaml}
    if [ -z "${kubeconfig}" ] || [ ! -s "${kubeconfig}" ]; then
        kubeconfig=${private}/kubeconfig
        # shellcheck disable=SC2310 # its failure is why nothing is asked of the management cluster.
        if ! tofu_output kubeconfig "${kubeconfig}"; then
            cp "${kubeconfig}.why" "${private}/machines.why"
            kubeconfig=''
            return 0
        fi
    fi
    # shellcheck disable=SC2310 # its failure is recorded.
    if ! mgmt_kubectl get proxmoxmachines -A -o json > "${private}/proxmoxmachines.json" 2> "${private}/machines.why"; then
        kubeconfig=''
        return 0
    fi
    jq -r '.items[] | select(.spec.virtualMachineID != null) | [.spec.virtualMachineID, .metadata.namespace,
            (.metadata.labels["cluster.x-k8s.io/cluster-name"] // "-"),
            ([.status.addresses[]? | select(.type == "InternalIP") | .address][0] // "-")] |
        map(tostring) | join(" ")' "${private}/proxmoxmachines.json" > "${private}/machines" 2> "${private}/machines.why" ||
        : > "${private}/machines"
}

# cluster_talosconfig <namespace> <cluster> <file>: the workload cluster's talosconfig from its
# Secret into the file, once, or why there is none into <file>.why.
cluster_talosconfig() {
    if [ -e "$3" ] || [ -e "$3.why" ]; then
        return 0
    fi
    if [ -z "${kubeconfig}" ]; then
        cp "${private}/machines.why" "$3.why"
        return 0
    fi
    # shellcheck disable=SC2310 # its failure is recorded.
    if mgmt_kubectl -n "$1" get secret "$2-talosconfig" -o jsonpath='{.data.talosconfig}' > "$3.base64" 2> "$3.why" &&
        base64 -d "$3.base64" > "$3" 2>> "$3.why" && [ -s "$3" ]; then
        rm -f "$3.why"
    else
        rm -f "$3"
        [ -s "$3.why" ] || echo "the Secret $2-talosconfig holds no talosconfig" > "$3.why"
    fi
    rm -f "$3.base64"
}

# agent_address <VM ID>: the VM's first IPv4 address as its guest agent reports it, past loopback and
# the links a CNI adds.
agent_address() {
    timeout -k 10 60 docker exec kubehenge-e2e-pve qm guest cmd "$1" network-get-interfaces < /dev/null 2> /dev/null |
        jq -r '[.[]? | select(.name | test("^(lo|cni|flannel|veth|cilium|lxc|kube-|vxlan|tunl|cali|docker|dummy)") | not) |
            ."ip-addresses"[]? | select(."ip-address-type" == "ipv4") | ."ip-address"][0] // empty' 2> /dev/null || true
}

# talos_vm <VM ID> <tags>: whether the VM runs Talos. If it does, sets talos_config to its cluster's
# talosconfig and talos_address to where its API answers, or talos_why to why it cannot be read.
talos_vm() {
    talos_config='' talos_address='' talos_why=''
    machine=$(awk -v id="$1" '$1 == id { print $2, $3, $4; exit }' "${private}/machines")
    read -r m_namespace m_cluster m_address << EOF
${machine}
EOF
    case ";$2;" in
        *";management;"*)
            talos_config=${private}/management.talosconfig
            if [ ! -e "${talos_config}" ] && [ ! -e "${talos_config}.why" ]; then
                # shellcheck disable=SC2310 # its failure is recorded.
                tofu_output talosconfig "${talos_config}" || true
            fi
            ;;
        *)
            if [ -n "${m_cluster}" ] && [ "${m_cluster}" != - ]; then
                talos_config=${private}/${m_namespace}_${m_cluster}.talosconfig
                cluster_talosconfig "${m_namespace}" "${m_cluster}" "${talos_config}"
            else
                talos_config=${private}/unknown.talosconfig
                {
                    echo "no Cluster API machine of the management cluster is this VM"
                    cat "${private}/machines.why" 2> /dev/null || true
                } > "${talos_config}.why"
            fi
            case ";$2;" in
                *";talos;"*) ;;
                *) [ -s "${talos_config}" ] || return 1 ;;
            esac
            ;;
    esac
    if [ ! -s "${talos_config}" ]; then
        talos_why="no talosconfig: $(cat "${talos_config}.why" 2> /dev/null || true)"
        return 0
    fi
    talos_address=${m_address}
    case ${talos_address} in
        '' | -) talos_address=$(agent_address "$1") ;;
        *) ;;
    esac
    [ -n "${talos_address}" ] || talos_why="neither Cluster API nor the guest agent gives its address"
}

collect() {
    out=$1 work=$2
    mkdir -p "${out}"
    private=$(mktemp -d)
    trap 'rm -rf "${private}"' EXIT
    trap 'exit 1' HUP INT TERM
    talos_machines
    docker exec kubehenge-e2e-pve qm list | awk 'NR > 1 && $3 == "running" { print $1, $2 }' |
        while read -r vmid name; do
            dir="${out}/${vmid}-${name}" && mkdir -p "${dir}"
            tags=$(timeout -k 10 60 docker exec kubehenge-e2e-pve qm config "${vmid}" < /dev/null 2> /dev/null |
                sed -n 's/^tags: //p' | tr ', ' ';;')
            # shellcheck disable=SC2310 # it returns whether the VM runs Talos.
            if talos_vm "${vmid}" "${tags}"; then
                if [ -n "${talos_why}" ]; then
                    printf 'Talos, whose guest agent runs no commands, cannot be read through its API: %s\n' \
                        "${talos_why}" | redact | head -c 1000000 > "${dir}/state.txt"
                else
                    talos_collect "${dir}"
                fi
                continue
            fi
            # A VM whose agent does not answer, or that has no shell, is tried once; its first part
            # says why.
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
    collect) collect "${2:?usage: $0 collect <directory> [<work directory>]}" "${3:-${RUNNER_TEMP:+${RUNNER_TEMP}/e2e}}" ;;
    *)
        echo "usage: $0 record <file> | summary <file> | collect <directory> [<work directory>]" >&2
        exit 2
        ;;
esac
