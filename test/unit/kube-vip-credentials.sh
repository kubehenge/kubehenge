#!/bin/sh
# Unit test of charts/kubehenge/files/kube-vip-credentials.sh, which moves kube-vip on the node
# kubeadm init runs on from super-admin.conf to admin.conf: stand-ins for systemctl, crictl, kubectl
# and sleep play the kubelet, kube-vip and the API server behind the virtual IP and record what the
# script does, and a copy of the script works on a manifest here instead of the node's.
#
# The stand-ins are shell functions and the copy runs in a subshell that sources it, as each process
# started costs most of this test's time on Windows. The copy retries a few times, not 180.
#
#   test/unit/kube-vip-credentials.sh
set -eu

here=$(dirname "$0")
chart=${here}/../../charts/kubehenge
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0 got=
# expect <what> <wanted> <command>...: whether the command sets got to what is wanted.
expect() {
    what=$1 wanted=$2
    shift 2
    got=
    "$@" || true
    if [ "${got}" = "${wanted}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "${what}" "${wanted}" "${got}"
        failures=$((failures + 1))
    fi
}

# The chart's kube-vip manifest mounts the kubeconfig at the path the script moves.
mounted() {
    got=$(
        grep -c -F 'path: {{ .kubeconfig }}' "${chart}/templates/_helpers.tpl"
        grep -c -F '"kubeconfig" "/etc/kubernetes/admin.conf"' "${chart}/templates/kubeadm.yaml"
    )
}
expect "kubeadm's kube-vip manifest mounts admin.conf from a path the script can move" "1
1" mounted

retries=9
sed -e "s#/etc/kubernetes/manifests/kube-vip.yaml#${root}/kube-vip.yaml#" \
    -e "s#/run/kubeadm/kubeadm.yaml#${root}/kubeadm.yaml#" \
    -e "s#^\( *\)tries=180\$#\1tries=${retries}#" \
    "${chart}/files/kube-vip-credentials.sh" > "${root}/credentials.sh"
has_retries=no
while IFS= read -r line; do
    case ${line} in
        *"tries=${retries}") has_retries=yes ;;
        *) ;;
    esac
done < "${root}/credentials.sh"
if [ "${has_retries}" = no ]; then
    echo 'FAIL: the copy of the script keeps its 180 tries, which a case that never ends would run through'
    exit 1
fi

# kubeconfig: sets got to the path under /etc/kubernetes the manifest names.
kubeconfig() {
    got=
    while IFS= read -r line; do
        line=${line#"${line%%[! ]*}"}
        case ${line} in
            'path: /etc/kubernetes/'*) got=${line#path: /etc/kubernetes/} ;;
            *) ;;
        esac
    done < "${root}/kube-vip.yaml"
}
# systemctl records each start and stop of the kubelet with the kubeconfig the manifest names at
# that moment. Once started, the kubelet replaces the running kube-vip after $root/handover seconds.
systemctl() {
    kubeconfig
    printf '%s %s: %s\n' "$1" "$2" "${got}" >> "${root}/events"
    if [ "$1" = start ]; then read -r countdown < "${root}/handover"; fi
}
sleep() {
    [ -n "${countdown}" ] || return 0
    countdown=$((countdown - 1))
    if [ "${countdown}" -le 0 ]; then
        countdown=
        echo new > "${root}/running"
    fi
}
crictl() {
    [ "$*" = "ps -q --state running --name ^kube-vip\$" ] || return 2
    read -r kube_vip < "${root}/running"
    printf '%s\n' "${kube_vip}"
}
# The API server answers through the virtual IP while a kube-vip holds it, unless $unanswered.
kubectl() {
    read -r kube_vip < "${root}/running"
    echo "readyz ${kube_vip:-none}" >> "${root}/events"
    [ -n "${kube_vip}" ] && [ -z "${unanswered}" ]
}

unanswered=
# node <first|joining> <running kube-vip, or ""> <seconds to hand over>: a node before the script.
node() {
    if [ -f "${root}/kubeadm.yaml" ]; then rm "${root}/kubeadm.yaml"; fi
    [ "$1" = joining ] || : > "${root}/kubeadm.yaml"
    unanswered=
    echo "$2" > "${root}/running"
    echo "$3" > "${root}/handover"
    : > "${root}/events"
    printf '  volumes:\n    - name: kubeconfig\n      hostPath:\n        path: /etc/kubernetes/admin.conf\n        type: File\n' > "${root}/kube-vip.yaml"
}
# run <init|done>: sets got to the script's exit status; what it said is in $root/said.
run() {
    if (
        countdown=
        set -- "$1"
        # shellcheck source=/dev/null
        . "${root}/credentials.sh"
    ) > "${root}/said" 2>&1; then
        got=0
    else
        got=$?
    fi
}
events() {
    got=
    while IFS= read -r line; do
        got=${got:+${got},}${line}
    done < "${root}/events"
}
said() { read -r got < "${root}/said"; }
running() { read -r got < "${root}/running"; }

node joining old 3
expect "a joining node: init succeeds" 0 run init
expect "a joining node: done succeeds" 0 run "done"
expect "a joining node keeps admin.conf" admin.conf kubeconfig
expect "a joining node's kubelet is left alone" "" events

node first old 3
expect "init succeeds" 0 run init
expect "init gives kube-vip super-admin.conf" super-admin.conf kubeconfig
expect "init leaves the kubelet alone" "" events

expect "done succeeds" 0 run "done"
expect "done gives kube-vip admin.conf" admin.conf kubeconfig
expect "the manifest changes while the kubelet is stopped, and the API is asked only once the old kube-vip is gone" \
    "stop kubelet: super-admin.conf,start kubelet: admin.conf,readyz new" events
expect "done returns with the new kube-vip running" new running

node first "" 2
run init
expect "done without a kube-vip running succeeds" 0 run "done"
expect "done without a kube-vip running waits for the new one to answer" \
    "stop kubelet: super-admin.conf,start kubelet: admin.conf,readyz none,readyz none,readyz new" events

node first old 1
run init
unanswered=yes
expect "done fails when the API never answers through the virtual IP" 1 run "done"
expect "and says so" "kube-vip has not taken the control-plane endpoint over on admin.conf" said

expect "an unknown step is refused" 2 run upgrade

echo "kube-vip-credentials: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
