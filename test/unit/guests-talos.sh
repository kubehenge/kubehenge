#!/bin/sh
# Unit test of how test/e2e/ci/guests.sh collect reads Talos VMs through Talos's API: stand-ins for
# docker (Proxmox VE's qm), kubectl, tofu and talosctl, and the VMs of a management cluster, of a
# workload cluster on Talos with a node that refuses and one that never answers, and of a cluster on
# kubeadm. Needs jq and no Proxmox VE.
#
#   test/unit/guests-talos.sh
set -eu

here=$(dirname "$0")
guests=${here}/../e2e/ci/guests.sh
root=$(mktemp -d)
bin=${root}/bin
mkdir "${bin}" "${root}/work" "${root}/work/mgmt" "${root}/tmp"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# has <what> <file> <text>: whether the file holds the text.
has() {
    if [ -f "$2" ] && grep -qF -- "$3" "$2"; then
        passes=$((passes + 1))
    else
        said='(no file)'
        [ ! -f "$2" ] || said=$(cat "$2")
        printf 'FAIL: %s\n  wanted %s to say: %s\n  it said: %s\n' "$1" "$2" "$3" "${said}"
        failures=$((failures + 1))
    fi
}
# lacks <what> <file> <text>: whether the file, if there is one, leaves the text out.
lacks() {
    if [ -f "$2" ] && grep -qF -- "$3" "$2"; then
        printf 'FAIL: %s\n  wanted %s not to say: %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    else
        passes=$((passes + 1))
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

# jq's Windows builds end their lines with CR LF, which the script's reads would keep.
real_jq=$(command -v jq)
cat > "${bin}/jq" << EOF
#!/bin/sh
"${real_jq}" "\$@" > "${root}/jq.out" || code=\$?
tr -d '\r' < "${root}/jq.out"
exit "\${code:-0}"
EOF

cat > "${bin}/docker" << 'EOF'
#!/bin/sh
[ "$1 $2 $3" = "exec kubehenge-e2e-pve qm" ] || {
    echo "unexpected: docker $*" >&2
    exit 1
}
shift 3
case $1 in
    list)
        echo '      VMID NAME                 STATUS     MEM(MB)    BOOTDISK(GB) PID'
        for vm in '100 mgmt-0' '101 talos-cp' '102 talos-worker' '103 talos-down' '104 talos-hang' '105 kubeadm-cp'; do
            echo "       ${vm} running 4096 30.00 1"
        done
        echo '       106 stopped-vm stopped 4096 30.00 0'
        ;;
    config)
        case $2 in
            100) echo 'tags: kubehenge;management' ;;
            101 | 103 | 104) echo 'tags: kubehenge;talos;v1.11' ;;
            *) echo 'tags: kubehenge' ;;
        esac
        ;;
    guest)
        case "$2 $3" in
            'cmd 100')
                echo '[{"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4","prefix":8}]},
                    {"name":"cni0","ip-addresses":[{"ip-address":"10.244.0.1","ip-address-type":"ipv4","prefix":24}]},
                    {"name":"ens18","ip-addresses":[{"ip-address":"fe80::1","ip-address-type":"ipv6","prefix":64},
                        {"ip-address":"172.20.99.11","ip-address-type":"ipv4","prefix":24}]}]'
                ;;
            'exec 105') echo '{"exitcode":0,"out-data":"guest 105 says hi\n"}' ;;
            *)
                echo 'QEMU guest agent is not running'
                exit 255
                ;;
        esac
        ;;
    *) exit 1 ;;
esac
EOF

cat > "${bin}/tofu" << EOF
#!/bin/sh
[ "\$1" = "-chdir=${root}/work/mgmt" ] && [ -d "${root}/work/mgmt" ] || {
    echo "Error: no state in \$1" >&2
    exit 1
}
case "\$2 \$3 \$4" in
    'output -raw talosconfig') printf 'context: mgmt\nkey: SECRET-MANAGEMENT-TALOSCONFIG\n' ;;
    'output -raw kubeconfig') printf 'management kubeconfig SECRET-KUBECONFIG\n' ;;
    *) exit 1 ;;
esac
EOF

cat > "${root}/proxmoxmachines.json" << 'EOF'
{"items": [
    {"metadata": {"namespace": "e2e", "labels": {"cluster.x-k8s.io/cluster-name": "talos-1"}},
     "spec": {"virtualMachineID": 101}, "status": {"addresses": [{"type": "Hostname", "address": "cp"},
        {"type": "InternalIP", "address": "172.20.99.21"}]}},
    {"metadata": {"namespace": "e2e", "labels": {"cluster.x-k8s.io/cluster-name": "talos-1"}},
     "spec": {"virtualMachineID": 102}, "status": {"addresses": [{"type": "InternalIP", "address": "172.20.99.22"}]}},
    {"metadata": {"namespace": "e2e", "labels": {"cluster.x-k8s.io/cluster-name": "talos-1"}},
     "spec": {"virtualMachineID": 103}, "status": {"addresses": [{"type": "InternalIP", "address": "172.20.99.23"}]}},
    {"metadata": {"namespace": "e2e", "labels": {"cluster.x-k8s.io/cluster-name": "talos-1"}},
     "spec": {"virtualMachineID": 104}, "status": {"addresses": [{"type": "InternalIP", "address": "172.20.99.24"}]}},
    {"metadata": {"namespace": "e2e", "labels": {"cluster.x-k8s.io/cluster-name": "kubeadm-1"}},
     "spec": {"virtualMachineID": 105}, "status": {"addresses": [{"type": "InternalIP", "address": "172.20.99.25"}]}},
    {"metadata": {"namespace": "e2e", "labels": {"cluster.x-k8s.io/cluster-name": "kubeadm-1"}},
     "spec": {}, "status": {}}
]}
EOF
cat > "${bin}/kubectl" << EOF
#!/bin/sh
[ "\$1" = --kubeconfig ] && grep -q 'management kubeconfig' "\$2" && [ "\$3" = --request-timeout=20s ] || {
    echo "error: not the management cluster's kubeconfig" >&2
    exit 1
}
shift 3
case "\$*" in
    'get proxmoxmachines -A -o json') cat "${root}/proxmoxmachines.json" ;;
    '-n e2e get secret talos-1-talosconfig -o jsonpath={.data.talosconfig}')
        printf 'context: talos-1\nkey: SECRET-WORKLOAD-TALOSCONFIG\n' | base64 | tr -d '\n'
        ;;
    *)
        echo "Error from server (NotFound): secrets \"\$4\" not found" >&2
        exit 1
        ;;
esac
EOF

cat > "${bin}/talosctl" << 'EOF'
#!/bin/sh
[ "$1" = --talosconfig ] && [ "$3" = --endpoints ] && [ "$5" = --nodes ] && [ "$4" = "$6" ] || {
    echo "error: not asked of one node directly: $*"
    exit 2
}
config=$2 node=$4
shift 6
cluster=talos-1
[ "${node}" != 172.20.99.11 ] || cluster=mgmt
grep -q "context: ${cluster}" "${config}" || {
    echo "rpc error: code = Unauthenticated desc = not the talosconfig of ${node}"
    exit 1
}
case ${node} in
    172.20.99.23)
        echo "rpc error: code = Unavailable desc = dial tcp ${node}:50000: connect: connection refused" >&2
        exit 1
        ;;
    172.20.99.24) exec sleep 30 ;;
    *) ;;
esac
services='apid containerd cri kubelet machined udevd'
[ "${node}" = 172.20.99.22 ] || services="${services} etcd trustd"
case "$*" in
    version) printf 'Client:\n\tTag: v1.11.0\nServer:\n\tNODE: %s\n\tTag: v1.11.0\n' "${node}" ;;
    services)
        echo 'NODE SERVICE STATE HEALTH LAST-CHANGE LAST-EVENT'
        for s in ${services}; do echo "${node} ${s} Running OK 1h Healthy"; done
        ;;
    'service '*) echo "ID $2 STATE Running EVENTS restarted 3 times" ;;
    'logs '*) echo "${node}: $2 log line, token: abcdefgh12345678" ;;
    'list /var/log/pods -r')
        echo 'NODE NAME'
        for f in . kube-system_kube-apiserver-cp_1 kube-system_kube-apiserver-cp_1/kube-apiserver \
            kube-system_kube-apiserver-cp_1/kube-apiserver/0.log kube-system_kube-flannel-cp_2/kube-flannel/2.log \
            kube-system_kube-flannel-cp_2/kube-flannel/1.log; do
            echo "${node} ${f}"
        done
        ;;
    'read '*) echo "content of $2 on ${node}" ;;
    dmesg) echo "${node}: kern: info: the kernel of ${node}" ;;
    'etcd members') echo "${node} members of etcd" ;;
    *) echo "${node} answers $*" ;;
esac
EOF
chmod +x "${bin}"/*

# collect_into <out> <work directory>: guests.sh collect with the stand-ins, whose exit code it prints.
collect_into() {
    code=0
    env PATH="${bin}:${PATH}" TMPDIR="${root}/tmp" KUBEHENGE_E2E_TALOSCTL_TIMEOUT=2 \
        sh "${guests}" collect "$1" "$2" > "${root}/collect.log" 2>&1 || code=$?
    echo "${code}"
}

out=${root}/out
code=$(collect_into "${out}" "${root}/work")
truth "collect succeeds although nodes fail" [ "${code}" = 0 ]
left=$(ls -A "${root}/tmp")
truth "the talosconfigs and kubeconfig are removed" [ -z "${left}" ]
truth "a stopped VM is left alone" [ ! -e "${out}/106-stopped-vm" ]
for f in "${out}"/*/*.txt; do
    lacks "the talosconfigs and kubeconfig stay out of ${f}" "${f}" SECRET-
done

m=${out}/100-mgmt-0
has "the management cluster's node, at the address its guest agent gives" "${m}/state.txt" "read through its API at 172.20.99.11"
has "its release" "${m}/state.txt" "Tag: v1.11.0"
has "its services" "${m}/state.txt" "172.20.99.11 kubelet Running"
has "a service's events" "${m}/state.txt" "ID etcd STATE Running EVENTS restarted 3 times"
has "its pressure" "${m}/state.txt" "== talosctl read /proc/pressure/memory"
has "its etcd members" "${m}/etcd.txt" "172.20.99.11 members of etcd"

c=${out}/101-talos-cp
has "a workload control plane, at its machine's address" "${c}/state.txt" "read through its API at 172.20.99.21"
has "its machine's stage" "${c}/state.txt" "== talosctl get machinestatus -o yaml"
has "the kubelet's containers" "${c}/state.txt" "== talosctl containers --kubernetes"
has "machined's log" "${c}/services.txt" "172.20.99.21: machined log line"
has "etcd's log" "${c}/services.txt" "== talosctl logs etcd --tail 10000"
has "trustd's log" "${c}/services.txt" "== talosctl logs trustd"
has "the CRI's log" "${c}/services.txt" "== talosctl logs cri"
lacks "a service outside the list" "${c}/services.txt" "logs udevd"
has "tokens redacted" "${c}/services.txt" "token: <redacted>"
lacks "no token" "${c}/services.txt" abcdefgh12345678
has "the API server's log" "${c}/logs.txt" "content of /var/log/pods/kube-system_kube-apiserver-cp_1/kube-apiserver/0.log on 172.20.99.21"
has "a restarted container's earlier run" "${c}/logs.txt" "content of /var/log/pods/kube-system_kube-flannel-cp_2/kube-flannel/1.log"
lacks "not its current run" "${c}/logs.txt" "kube-flannel/2.log"
has "the kernel's log" "${c}/kernel.txt" "the kernel of 172.20.99.21"
has "etcd's status" "${c}/etcd.txt" "== talosctl etcd status"
has "etcd's alarms" "${c}/etcd.txt" "== talosctl etcd alarm list"
has "each call's exit code" "${c}/etcd.txt" "exit code: 0"

w=${out}/102-talos-worker
has "a worker without a talos tag, by its cluster's talosconfig" "${w}/state.txt" "read through its API at 172.20.99.22"
has "the kubelet's log" "${w}/services.txt" "== talosctl logs kubelet"
lacks "no etcd log on a worker" "${w}/services.txt" "logs etcd"
truth "no etcd part on a worker" [ ! -e "${w}/etcd.txt" ]

d=${out}/103-talos-down
has "a node that refuses says why" "${d}/state.txt" "connection refused"
has "and is asked nothing more" "${d}/state.txt" "nothing more was asked of it"
truth "so it has no other parts" [ ! -e "${d}/services.txt" ]

h=${out}/104-talos-hang
has "a node that never answers is cut off" "${h}/state.txt" "exit code: none, cut off after 2 s"
truth "and has no other parts" [ ! -e "${h}/services.txt" ]

k=${out}/105-kubeadm-cp
has "a VM of a cluster without a talosconfig is read through its guest agent" "${k}/state.txt" "guest 105 says hi"

# Without the run's work directory, a VM known to run Talos says why it is not read, and the others
# are read through their guest agents.
out=${root}/out-alone
code=$(collect_into "${out}" "${root}/nowhere")
truth "collect succeeds without a work directory" [ "${code}" = 0 ]
has "the management cluster's node says why" "${out}/100-mgmt-0/state.txt" "no talosconfig: no Terraform/OpenTofu state in '${root}/nowhere/mgmt'"
has "a VM tagged talos says why" "${out}/101-talos-cp/state.txt" "cannot be read through its API"
has "an untagged VM falls back to its guest agent" "${out}/102-talos-worker/state.txt" "QEMU guest agent is not running"
has "the kubeadm VM is still read" "${out}/105-kubeadm-cp/state.txt" "guest 105 says hi"

echo "guests-talos: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
