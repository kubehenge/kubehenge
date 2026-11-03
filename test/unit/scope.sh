#!/bin/sh
# Unit test of test/e2e/ci/scope.sh, which picks a merge group's end-to-end tests from the paths it
# changes: each of the nightly run's four extra tests comes with the files it exercises, and with a
# path no rule names.
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
for t in rancher two-nodes control-plane-ha management-cluster-upgrade; do
    expect pkg/provisioner/cluster.go "${t}" yes
    expect charts/kubehenge/values.yaml "${t}" yes
    expect test/e2e/common.sh "${t}" yes
    expect docs/testing.md "${t}" no
    expect test/e2e/workload-cluster.sh "${t}" no
    expect test/e2e/ci/nightly.sh "${t}" no
done

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
