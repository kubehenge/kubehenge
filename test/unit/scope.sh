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
# expect <path> <test> <yes|no>: whether the lanes for a change to the path run the test. A lane
# is a test or a pair, as in k3s-etcd,cluster-mesh.
expect() {
    list=$(printf '%s\n' "$1" | sh "${ci}/scope.sh")
    if printf '%s' "${list}" | jq -e --arg t "$2" 'any(.[]; split(",") | index($t) != null)' > /dev/null; then got=yes; else got=no; fi
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
expect charts/kubehenge/rancher_test.go rancher yes
expect charts/kubehenge/rancher_test.go k3s-etcd no
expect charts/kubehenge/chart_test.go rancher no
expect pkg/provisioner/cluster_test.go rancher no
# A test that shares a lane brings the lane: its own script runs the test it shares it with too.
expect test/e2e/cluster-mesh.sh cluster-mesh yes
expect test/e2e/cluster-mesh.sh k3s-etcd yes
expect test/e2e/cluster-mesh.sh k3s-machines no
expect test/e2e/autoscaler.sh autoscaler yes
expect test/e2e/autoscaler.sh k3s-addons-b yes
expect test/e2e/autoscaler.sh k3s-addons-a no
expect test/e2e/appliance.sh appliance yes
expect test/e2e/appliance.sh management-cluster yes
expect test/e2e/management-cluster.sh appliance yes
expect test/e2e/workload-cluster.sh cluster-mesh yes
expect test/e2e/workload-cluster.sh appliance no
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
    if printf '%s' "${list}" | jq -e 'index("k3s-etcd,cluster-mesh") != null' > /dev/null; then :; else
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
all=$(sh "${ci}/scope.sh" --all)
if printf '%s' "${all}" | jq -e 'index("rancher") == null' > /dev/null; then :; else
    echo 'FAIL: --all lists rancher'
    failures=$((failures + 1))
fi
# It holds 20 lanes at most, as many as a run's jobs at once, with every pair, and each test in one
# lane only.
if printf '%s' "${all}" | jq -e 'length <= 20' > /dev/null; then :; else
    printf 'FAIL: --all has over 20 lanes: %s\n' "${all}"
    failures=$((failures + 1))
fi
for pair in appliance,management-cluster k3s-etcd,cluster-mesh k3s-addons-b,autoscaler; do
    if printf '%s' "${all}" | jq -e --arg p "${pair}" 'index($p) != null' > /dev/null; then :; else
        printf 'FAIL: --all has no lane %s: %s\n' "${pair}" "${all}"
        failures=$((failures + 1))
    fi
done
if printf '%s' "${all}" | jq -e '[.[] | split(",")[]] | length == (unique | length)' > /dev/null; then :; else
    printf 'FAIL: --all runs a test in two lanes: %s\n' "${all}"
    failures=$((failures + 1))
fi
# A pair's tests share their templates: the lane's later test needs none its first does not.
# templates_of <test>: the templates.sh function of that name, each line's trailing blanks cut.
templates_of() {
    # shellcheck disable=SC2016 # expanded by the inner shell
    sh -c '. "$1/templates.sh"; node_os= from=v0 to=v0; templates_of "$2"' sh "${ci}" "$1" | sed 's/ *$//'
}
for pair in $(printf '%s' "${all}" | jq -r '.[] | select(contains(","))' | tr -d '\r'); do
    first=$(templates_of "${pair%%,*}")
    second=$(templates_of "${pair#*,}")
    if [ -z "${second}" ] || [ "${second}" = "${first}" ]; then :; else
        printf 'FAIL: %s: %s needs templates (%s) that %s does not make\n' "${pair}" "${pair#*,}" "${second}" "${pair%%,*}"
        failures=$((failures + 1))
    fi
done

if [ "${failures}" -gt 0 ]; then
    printf '%s failed\n' "${failures}"
    exit 1
fi
echo 'scope: ok'
