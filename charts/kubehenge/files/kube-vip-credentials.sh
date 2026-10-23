#!/bin/sh
# kube-vip-credentials.sh init|done: kube-vip's credentials on the node kubeadm init runs on.
#
# kubeadm init grants admin.conf its rights through the API server that kube-vip has yet to put
# behind the virtual IP, so kube-vip starts there with super-admin.conf (kube-vip/kube-vip#684), and
# moves to admin.conf once init is done rather than keep credentials that bypass authorisation for
# the life of the node. Nodes that join start on admin.conf and are left as they are.
set -eu
manifest=/etc/kubernetes/manifests/kube-vip.yaml
[ -f /run/kubeadm/kubeadm.yaml ] || exit 0
case "${1:-}" in
    init)
        sed -i 's#path: /etc/kubernetes/admin.conf#path: /etc/kubernetes/super-admin.conf#' "${manifest}"
        ;;
    done)
        # kubeadm init ends by restarting the kubelet. A kubelet that has yet to hear from the API
        # server drops the removal of a static pod whose manifest changes, and leaves the old pod
        # waiting for a volume it will never mount: kube-vip on super-admin.conf would run on for
        # two minutes and then leave the virtual IP unanswered until its successor starts, under
        # whatever uses the cluster by then. Changed while the kubelet is stopped, the manifest is
        # all the next kubelet knows of, and it stops the old kube-vip as soon as it is up, while the
        # node is still bootstrapping. The node is done once the new kube-vip holds the address.
        old=$(crictl ps -q --state running --name '^kube-vip$')
        systemctl stop kubelet
        sed -i 's#path: /etc/kubernetes/super-admin.conf#path: /etc/kubernetes/admin.conf#' "${manifest}"
        systemctl start kubelet
        tries=180
        while :; do
            gone=true
            if [ -n "${old}" ] && crictl ps -q --state running --name '^kube-vip$' | grep -qxF "${old}"; then
                gone=false
            fi
            if [ "${gone}" = true ] &&
                kubectl --kubeconfig=/etc/kubernetes/admin.conf --request-timeout=5s get --raw=/readyz > /dev/null 2>&1; then
                break
            fi
            tries=$((tries - 1))
            if [ "${tries}" -eq 0 ]; then
                echo "kube-vip has not taken the control-plane endpoint over on admin.conf" >&2
                exit 1
            fi
            sleep 1
        done
        ;;
    *)
        echo "usage: $0 init|done" >&2
        exit 2
        ;;
esac
