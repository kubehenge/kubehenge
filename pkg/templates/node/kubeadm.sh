#!/bin/sh
# Sets up a node's containerd and kubelet for kubeadm, as Kubernetes' installation guide does, under
# SELinux enforcing, once their packages are in. Run by the family's node.sh, beside this file:
#
#   kubeadm.sh
set -eu

files=$(dirname "$(dirname "$0")")

# kube-proxy reads conntrack's settings when it starts, before anything else loads the module.
printf 'overlay\nbr_netfilter\nnf_conntrack\n' > /usr/lib/modules-load.d/kubernetes.conf
printf 'net.ipv4.ip_forward = 1\nnet.bridge.bridge-nf-call-iptables = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\n' \
    > /usr/lib/sysctl.d/90-kubernetes.conf

mkdir -p /etc/containerd
containerd config default |
    sed -e 's/SystemdCgroup = false/SystemdCgroup = true/' -e 's/enable_selinux = false/enable_selinux = true/' \
        > /etc/containerd/config.toml
grep -q 'SystemdCgroup = true' /etc/containerd/config.toml
grep -q 'enable_selinux = true' /etc/containerd/config.toml
printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\n' \
    > /etc/crictl.yaml
install -D -m 0755 "${files}/node/images.sh" /usr/libexec/kubehenge/images

# The control plane's static pods read the cluster's keys and kubeconfigs in /etc/kubernetes and
# keep etcd's data in /var/lib/etcd, which the policy gives types no confined container may touch,
# so they run as SELinux's super-privileged container type, as the kubelet itself is unconfined;
# every other pod stays confined. kubeadm takes these from the patches directory the cluster's
# class names.
mkdir -p /etc/kubernetes/patches
for component in etcd kube-apiserver kube-controller-manager kube-scheduler; do
    printf 'spec:\n  securityContext:\n    seLinuxOptions:\n      type: spc_t\n' \
        > "/etc/kubernetes/patches/${component}+strategic.yaml"
done

# The CIS benchmark asks for the kubelet's service files to be root's alone.
chmod 0600 /usr/lib/systemd/system/kubelet.service /usr/lib/systemd/system/kubelet.service.d/*.conf

systemctl enable containerd kubelet
