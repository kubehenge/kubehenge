#!/bin/sh
# Unit test of charts/kubehenge/files/kube-vip-credentials.sh, which moves kube-vip on the node
# kubeadm init runs on from super-admin.conf to admin.conf: stand-ins for systemctl, crictl, kubectl
# and sleep play the kubelet, kube-vip and the API server behind the virtual IP and record what the
# script does, and a copy of the script works on a manifest here instead of the node's.
#
#   test/unit/kube-vip-credentials.sh
set -eu

here=$(dirname "$0")
chart=${here}/../../charts/kubehenge
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT
bin=${root}/bin
mkdir "${bin}"

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

# The chart's kube-vip manifest mounts the kubeconfig at the path the script moves.
mounted() {
    grep -c -F 'path: {{ .kubeconfig }}' "${chart}/templates/_helpers.tpl"
    grep -c -F '"kubeconfig" "/etc/kubernetes/admin.conf"' "${chart}/templates/kubeadm.yaml"
}
expect "kubeadm's kube-vip manifest mounts admin.conf from a path the script can move" "1
1" mounted

sed -e "s#/etc/kubernetes/manifests/kube-vip.yaml#${root}/kube-vip.yaml#" \
    -e "s#/run/kubeadm/kubeadm.yaml#${root}/kubeadm.yaml#" \
    "${chart}/files/kube-vip-credentials.sh" > "${root}/credentials.sh"

# systemctl records each start and stop of the kubelet with the kubeconfig the manifest names at
# that moment. Once started, the kubelet replaces the running kube-vip after $root/handover seconds.
cat > "${bin}/systemctl" << EOF
#!/bin/sh
printf '%s %s: %s\n' "\$1" "\$2" "\$(sed -n 's#^ *path: /etc/kubernetes/##p' "${root}/kube-vip.yaml")" >> "${root}/events"
if [ "\$1" = start ]; then cp "${root}/handover" "${root}/countdown"; fi
EOF
cat > "${bin}/sleep" << EOF
#!/bin/sh
[ -f "${root}/countdown" ] || exit 0
left=\$((\$(cat "${root}/countdown") - 1))
if [ "\${left}" -gt 0 ]; then
    echo "\${left}" > "${root}/countdown"
else
    rm "${root}/countdown"
    echo new > "${root}/running"
fi
EOF
cat > "${bin}/crictl" << EOF
#!/bin/sh
[ "\$*" = "ps -q --state running --name ^kube-vip\$" ] || exit 2
cat "${root}/running"
EOF
# The API server answers through the virtual IP while a kube-vip holds it, unless $root/unanswered.
cat > "${bin}/kubectl" << EOF
#!/bin/sh
running=\$(cat "${root}/running")
echo "readyz \${running:-none}" >> "${root}/events"
[ -n "\${running}" ] && [ ! -f "${root}/unanswered" ]
EOF
chmod +x "${bin}/systemctl" "${bin}/sleep" "${bin}/crictl" "${bin}/kubectl"
PATH=${bin}:${PATH}

# node <first|joining> <running kube-vip, or ""> <seconds to hand over>: a node before the script.
node() {
    rm -f "${root}/events" "${root}/countdown" "${root}/unanswered" "${root}/kubeadm.yaml"
    [ "$1" = joining ] || : > "${root}/kubeadm.yaml"
    echo "$2" > "${root}/running"
    echo "$3" > "${root}/handover"
    : > "${root}/events"
    printf '  volumes:\n    - name: kubeconfig\n      hostPath:\n        path: /etc/kubernetes/admin.conf\n        type: File\n' > "${root}/kube-vip.yaml"
}
# run <init|done>: the script's exit status; what it said is in $root/said.
run() {
    if sh "${root}/credentials.sh" "$1" > "${root}/said" 2>&1; then
        echo 0
    else
        echo "$?"
    fi
}
kubeconfig() { sed -n 's#^ *path: /etc/kubernetes/##p' "${root}/kube-vip.yaml"; }
events() { paste -sd , "${root}/events"; }
said() { cat "${root}/said"; }
running() { cat "${root}/running"; }

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
run init > /dev/null
expect "done without a kube-vip running succeeds" 0 run "done"
expect "done without a kube-vip running waits for the new one to answer" \
    "stop kubelet: super-admin.conf,start kubelet: admin.conf,readyz none,readyz none,readyz new" events

node first old 1
run init > /dev/null
: > "${root}/unanswered"
expect "done fails when the API never answers through the virtual IP" 1 run "done"
expect "and says so" "kube-vip has not taken the control-plane endpoint over on admin.conf" said

expect "an unknown step is refused" 2 run upgrade

echo "kube-vip-credentials: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
