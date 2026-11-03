#!/bin/sh
# Unit test of test/e2e/ci/scope.sh, which picks a merge group's end-to-end tests from the paths it
# changes: each of the nightly run's four extra tests comes with its own script and the code only it
# exercises, and a path no rule names brings every other test.
#
#   test/unit/scope.sh
set -eu

here=$(dirname "$0")
ci=${here}/../e2e/ci

failures=0
# expect <path> <test> <yes|no>: whether the tests for a change to the path include the test.
expect() {
    list=$(printf '%s\n' "$1" | sh "${ci}/scope.sh")
    if printf '%s' "${list}" | jq -e --arg t "$2" 'index($t) != null' > /dev/null; then got=yes; else got=no; fi
    if [ "${got}" != "$3" ]; then
        printf 'FAIL: %s: %s in %s, want %s\n' "$1" "$2" "${list}" "$3"
        failures=$((failures + 1))
    fi
}

expect test/e2e/rancher.sh rancher yes
expect test/e2e/rancher.sh two-nodes no
expect test/e2e/two-nodes.sh two-nodes yes
expect test/e2e/ci/second-node.sh two-nodes yes
expect test/e2e/ci/second-node.sh rancher no
expect test/e2e/control-plane-ha.sh control-plane-ha yes
expect test/e2e/control-plane-ha.sh rancher no
expect test/e2e/management-cluster-upgrade.sh management-cluster-upgrade yes
expect test/e2e/management-cluster-upgrade.sh management-cluster no
expect test/e2e/lib.sh management-cluster-upgrade yes
expect test/e2e/management-cluster.sh management-cluster-upgrade yes
expect pkg/provisioner/rancher.go rancher yes
expect pkg/provisioner/rancher.go k3s-etcd yes
expect pkg/provisioner/rancher.go k3s-machines yes
expect test/e2e/workload-cluster.sh rke2-etcd yes
expect test/e2e/workload-cluster.sh rke2-machines yes
expect pkg/provisioner/hooks.go rancher yes
expect pkg/provisioner/placement.go two-nodes yes
expect pkg/provisioner/placement.go rancher no
expect pkg/provisioner/hagroups.go two-nodes yes
expect charts/kubehenge/files/kube-vip-credentials.sh control-plane-ha yes
expect management-cluster/versions.tf management-cluster-upgrade yes
expect management-cluster/versions.tf kubeadm yes
expect pkg/provisioner/operator.go management-cluster-upgrade yes
expect scripts/helm-addon-patches/pin-charts-by-digest.patch management-cluster-upgrade yes
expect pkg/provisioner/turtles.go rancher yes
expect pkg/provisioner/turtles.go k3s-etcd yes
expect pkg/provisioner/rancher_test.go rancher yes
expect pkg/provisioner/turtles_test.go rancher yes
expect pkg/provisioner/cluster_test.go rancher no
# Without a base to compare with, any change to the values may touch Rancher's.
expect charts/kubehenge/values.yaml rancher yes
for t in rancher two-nodes control-plane-ha management-cluster-upgrade; do
    expect pkg/provisioner/cluster.go "${t}" no
    [ "${t}" = rancher ] || expect charts/kubehenge/values.yaml "${t}" no
    expect test/e2e/common.sh "${t}" no
    expect docs/testing.md "${t}" no
    expect test/e2e/workload-cluster.sh "${t}" no
    expect test/e2e/ci/nightly.sh "${t}" no
done

# With a base, the values bring Rancher's test only for a change to addons.rancher. The base is a
# tree of a repository made for the test, the change the checkout's copy of the values.
repo=$(mktemp -d)
trap 'rm -rf "${repo}"' EXIT
mkdir -p "${repo}/charts/kubehenge"
cp "${here}/../../charts/kubehenge/values.yaml" "${repo}/charts/kubehenge/values.yaml"
git -C "${repo}" init -q
git -C "${repo}" add charts/kubehenge/values.yaml
tree=$(git -C "${repo}" write-tree)
values=${repo}/charts/kubehenge/values.yaml
scope_dir=$(cd "${ci}" && pwd)
# expect_values <case> <want>: whether the checkout's values, against the tree, bring Rancher's test.
expect_values() {
    list=$(cd "${repo}" && printf 'charts/kubehenge/values.yaml\n' | sh "${scope_dir}/scope.sh" --base "${tree}")
    if printf '%s' "${list}" | jq -e 'index("rancher") != null' > /dev/null; then got=yes; else got=no; fi
    if [ "${got}" != "$2" ]; then
        printf 'FAIL: values.yaml %s: rancher in %s, want %s\n' "$1" "${list}" "$2"
        failures=$((failures + 1))
    fi
    if printf '%s' "${list}" | jq -e 'index("k3s-etcd") != null' > /dev/null; then :; else
        printf 'FAIL: values.yaml %s: no k3s-etcd in %s\n' "$1" "${list}"
        failures=$((failures + 1))
    fi
}
original=$(cat "${values}")
expect_values unchanged no
# Rancher's chart pin, as Renovate raises it.
printf '%s\n' "${original}" | sed 's|^\(    version: \)2\.[0-9.]*@|\19.9.9@|' > "${values}"
expect_values 'with addons.rancher changed' yes
# A pin in the Turtles block nested inside it.
printf '%s\n' "${original}" | sed 's|turtles:v[0-9.]*@|turtles:v9.9.9@|' > "${values}"
expect_values "with Rancher's Turtles changed" yes
# Headlamp's pin, the block before Rancher's, and Prometheus's comment, the line after it.
printf '%s\n' "${original}" | sed 's|depName=ghcr.io/headlamp-k8s/headlamp|depName=ghcr.io/headlamp-k8s/headlamp2|; s|^  # Prometheus, Alertmanager|  # Prometheus and Alertmanager|' > "${values}"
expect_values 'changed outside addons.rancher' no

# --all stays the merge queue's set, without the nightly run's four.
if sh "${ci}/scope.sh" --all | jq -e 'index("rancher") == null' > /dev/null; then :; else
    echo 'FAIL: --all lists rancher'
    failures=$((failures + 1))
fi

if [ "${failures}" -gt 0 ]; then
    printf '%s failed\n' "${failures}"
    exit 1
fi
echo 'scope: ok'
