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

expect test/e2e/appliance-upgrade.sh appliance-upgrade yes
expect test/e2e/appliance-upgrade.sh appliance no
expect test/e2e/appliance.sh appliance-upgrade no
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
for t in rancher two-nodes control-plane-ha management-cluster-upgrade appliance-upgrade; do
    expect pkg/provisioner/cluster.go "${t}" no
    [ "${t}" = rancher ] || expect charts/kubehenge/values.yaml "${t}" no
    expect test/e2e/common.sh "${t}" no
    expect docs/testing.md "${t}" no
    expect test/e2e/workload-cluster.sh "${t}" no
    expect test/e2e/ci/nightly.sh "${t}" no
done

# expect_lanes <paths, one per line> <lanes>: the lanes, in order, as compact JSON.
expect_lanes() {
    list=$(printf '%s\n' "$1" | sh "${ci}/scope.sh" | jq -c . | tr -d '\r')
    if [ "${list}" != "$2" ]; then
        printf 'FAIL: %s: %s, want %s\n' "$1" "${list}" "$2"
        failures=$((failures + 1))
    fi
}
# expect_all <paths>: every lane of the merge queue.
expect_all() {
    want=$(sh "${ci}/scope.sh" --all | jq -c . | tr -d '\r')
    expect_lanes "$1" "${want}"
}
# expect_each <path> <yes|no> <test>...: whether the lanes for a change to the path run each test.
expect_each() {
    each_path=$1
    each_want=$2
    shift 2
    each_list=$(printf '%s\n' "${each_path}" | sh "${ci}/scope.sh")
    each_wrong=$(printf '%s' "${each_list}" | jq -r --arg want "${each_want}" \
        '[.[] | split(",")[]] as $s | $ARGS.positional[] as $t | select((if $s | index($t) then "yes" else "no" end) != $want) | $t' \
        --args "$@" | tr -d '\r' | tr '\n' ' ')
    if [ -n "${each_wrong}" ]; then
        printf 'FAIL: %s: %s in %s, want %s\n' "${each_path}" "${each_wrong}" "${each_list}" "${each_want}"
        failures=$((failures + 1))
    fi
}
appliance_lane='"appliance,management-cluster"'
workload_tests='kubeadm kubeadm-addons-a kubeadm-addons-b kubeadm-addons-c rke2-etcd rke2-machines rke2-addons-a rke2-addons-b rke2-addons-c rke2-addons-d k3s-etcd k3s-machines k3s-addons-a k3s-addons-b talos talos-addons-a talos-addons-b talos-addons-c'

# The web interface runs in the appliance alone, and its tests and tooling in no lane.
expect_lanes web/src/app.tsx "[${appliance_lane}]"
expect_lanes web/package-lock.json "[${appliance_lane}]"
expect_lanes web/src/app.test.tsx '[]'
expect_lanes web/src/a11y.test.ts '[]'
expect_lanes web/screenshots.ts '[]'
expect_lanes test/unit/scope.sh '[]'
expect_lanes scripts/changelog.sh '[]'
expect_lanes docs/changes/node-identity.md '[]'
expect_lanes test/unit/guests-trim.sh '[]'
expect_lanes test/e2e/ci/scope.sh '[]'
expect_lanes web/stylelint.config.mjs '[]'
# The interface's code runs in the appliance and in the workload test that acts through it.
expect_lanes pkg/ui/clusters.go "[${appliance_lane},\"k3s-machines\"]"
expect_lanes pkg/cli/serve.go "[${appliance_lane},\"k3s-machines\"]"
expect_lanes pkg/cli/cluster.go "[${appliance_lane},\"k3s-machines\"]"
expect_lanes pkg/ui/clusters_test.go '[]'
expect_lanes pkg/cli/cluster_test.go '[]'
expect_lanes pkg/appliance/testdata/tofu/main.tf "[${appliance_lane}]"
# The appliance's own commands.
for path in pkg/cli/appliance.go pkg/cli/activity.go pkg/cli/notifications.go pkg/cli/update.go pkg/appliance/jobs.go \
    appliance/install.sh appliance/pve-entry/entry.js test/e2e/appliance.sh pkg/appliance/e2e_test.go; do
    expect_lanes "${path}" "[${appliance_lane}]"
done
# The Go tests that need a Proxmox VE host run in the lane of what they test.
expect_lanes pkg/templates/e2e_test.go "[${appliance_lane}]"
expect_lanes pkg/provisioner/e2e_test.go "[${appliance_lane}]"
# The support bundle comes from every workload test, and from the interface.
for path in pkg/diagnose/diagnose.go pkg/cli/diagnose.go; do
    # shellcheck disable=SC2086 # one test per word
    expect_each "${path}" yes appliance management-cluster ${workload_tests} cluster-mesh autoscaler
    expect_each "${path}" no node-image-refresh rancher two-nodes control-plane-ha management-cluster-upgrade
done
for path in pkg/cli/etcdsnapshot.go test/e2e/cis-benchmark.sh; do
    # shellcheck disable=SC2086 # one test per word
    expect_each "${path}" yes ${workload_tests}
    expect_each "${path}" no appliance management-cluster node-image-refresh rancher two-nodes control-plane-ha
done
# A helper two lanes source brings both: the workload tests and, with them, the pair of k3s-etcd.
# shellcheck disable=SC2086 # one test per word
expect_each test/e2e/backups.sh yes ${workload_tests} cluster-mesh autoscaler
expect_each test/e2e/backups.sh no appliance node-image-refresh rancher
# A ClusterClass brings the tests that make a cluster of it, and the tests that install the chart.
expect_each charts/kubehenge/templates/kubeadm.yaml yes appliance management-cluster kubeadm kubeadm-addons-a kubeadm-addons-b kubeadm-addons-c
expect_each charts/kubehenge/templates/kubeadm.yaml no rke2-etcd k3s-etcd talos node-image-refresh cluster-mesh rancher management-cluster-upgrade
expect_each charts/kubehenge/templates/rke2.yaml yes appliance management-cluster rke2-etcd rke2-machines rke2-addons-a rke2-addons-b rke2-addons-c rke2-addons-d
expect_each charts/kubehenge/templates/rke2.yaml no kubeadm k3s-etcd talos node-image-refresh cluster-mesh
expect_each charts/kubehenge/templates/talos.yaml yes appliance management-cluster talos talos-addons-a talos-addons-b talos-addons-c
expect_each charts/kubehenge/templates/talos.yaml no kubeadm rke2-etcd k3s-etcd node-image-refresh cluster-mesh
# The k3s class is also the class of the tests that make a plain cluster.
expect_each charts/kubehenge/templates/k3s.yaml yes appliance management-cluster k3s-etcd k3s-machines k3s-addons-a k3s-addons-b cluster-mesh autoscaler node-image-refresh
expect_each charts/kubehenge/templates/k3s.yaml no kubeadm rke2-etcd talos rancher two-nodes control-plane-ha management-cluster-upgrade

# A path no rule names, and the code every lane runs, bring every lane.
expect_all pkg/provisioner/cluster.go
expect_all pkg/templates/talos.go
expect_all pkg/cli/controller.go
expect_all pkg/cli/root.go
expect_all pkg/cli/csiproxy.go
expect_all pkg/csiproxy/proxy.go
expect_all charts/kubehenge/templates/_helpers.tpl
expect_all charts/kubehenge/templates/controller.yaml
expect_all charts/kubehenge/templates/autoscaler.yaml
expect_all charts/kubehenge/Chart.yaml
expect_all "$(printf 'charts/kubehenge/Chart.yaml\ndocs/changelog.md')"
expect_all go.mod
expect_all Makefile
expect_all .github/workflows/e2e.yml
expect_all test/e2e/ci/run.sh
expect_all brand-new/file.txt
# A mix gives the union of its paths' lanes.
expect_lanes "$(printf 'web/src/app.tsx\npkg/cli/cluster.go')" "[${appliance_lane},\"k3s-machines\"]"
expect_lanes "$(printf 'web/src/app.tsx\ndocs/testing.md')" "[${appliance_lane}]"
expect_lanes "$(printf 'docs/testing.md\npkg/ui/clusters.go\nREADME.md')" "[${appliance_lane},\"k3s-machines\"]"
expect_lanes "$(printf 'test/e2e/autoscaler.sh\ntest/e2e/node-image-refresh.sh')" '["node-image-refresh","k3s-addons-b,autoscaler"]'
expect_lanes "$(printf 'test/e2e/rancher.sh\npkg/ui/clusters.go')" "[${appliance_lane},\"k3s-machines\",\"rancher\"]"
expect_each "$(printf 'charts/kubehenge/templates/kubeadm.yaml\ncharts/kubehenge/templates/rke2.yaml')" yes kubeadm rke2-etcd appliance
expect_each "$(printf 'charts/kubehenge/templates/kubeadm.yaml\ncharts/kubehenge/templates/rke2.yaml')" no talos k3s-etcd
# One path no rule names brings every lane, whatever else changes.
expect_all "$(printf 'web/src/app.tsx\npkg/provisioner/cluster.go\ndocs/testing.md')"
expect_all "$(printf 'docs/testing.md\nbrand-new/file.txt')"
# Paths with spaces are one path each, and a quoted path, which git prints for odd bytes, is unknown.
expect_lanes 'docs/my file.md' '[]'
expect_lanes 'web/src/my page.tsx' "[${appliance_lane}]"
expect_lanes 'pkg/ui/my file.go' "[${appliance_lane},\"k3s-machines\"]"
expect_all 'weird dir/file.txt'
expect_all '"docs/caf\303\251.md"'
# No change, or only blank lines, runs nothing.
expect_lanes '' '[]'
expect_lanes "$(printf '\n\n')" '[]'
empty=$(printf '' | sh "${ci}/scope.sh")
if [ "${empty}" != '[]' ]; then
    echo 'FAIL: an empty diff runs a lane'
    failures=$((failures + 1))
fi
# A deleted file is listed by its path, as a change is. A rename, listed with --no-renames, names
# both paths, so a ClusterClass moved away still brings its tests.
expect_each test/e2e/autoscaler.sh yes autoscaler
expect_lanes "$(printf 'docs/old.md\ndocs/new.md')" '[]'
expect_each "$(printf 'charts/kubehenge/templates/talos.yaml\ndocs/talos.yaml')" yes talos talos-addons-c
expect_each "$(printf 'test/e2e/rancher.sh\ntest/e2e/rancher-new.sh')" yes kubeadm
grep -q 'diff --name-only --no-renames' "${here}/../../.github/workflows/e2e.yml" || {
    echo 'FAIL: the workflow lists the diff with renames, which names only the new path'
    failures=$((failures + 1))
}
# The diff the workflow lists, of a repository where a ClusterClass moves into docs/.
moved=$(mktemp -d)
mkdir -p "${moved}/charts/kubehenge/templates" "${moved}/docs"
echo a > "${moved}/charts/kubehenge/templates/talos.yaml"
git -C "${moved}" init -q
git -C "${moved}" add .
before=$(git -C "${moved}" write-tree)
git -C "${moved}" mv charts/kubehenge/templates/talos.yaml docs/talos.yaml
after=$(git -C "${moved}" write-tree)
diffed=$(git -C "${moved}" -c core.quotepath=off diff --name-only --no-renames "${before}" "${after}" | tr -d '\r')
rm -rf "${moved}"
expect_each "${diffed}" yes talos talos-addons-a

# Every test a rule names is one the nightly run has: scope.sh refuses an unknown name, and the
# nightly run holds all of them.
nightly_tests=$(sh "${ci}/nightly.sh" 20000 | jq -r '.[] | split(",")[] | split("+")[0]' | tr -d '\r' | sort -u)
for t in $(sh "${ci}/scope.sh" --all | jq -r '.[] | split(",")[]' | tr -d '\r') management-cluster-upgrade rancher control-plane-ha two-nodes appliance-upgrade; do
    case "
${nightly_tests}
" in
        *"
${t}
"*) ;;
        *)
            printf 'FAIL: nightly.sh has no test %s\n' "${t}"
            failures=$((failures + 1))
            ;;
    esac
done
typo=$(mktemp -d)
# shellcheck disable=SC2016 # the pattern is the script's text
sed 's|need="${need} autoscaler"|need="${need} autoscalar"|' "${ci}/scope.sh" > "${typo}/scope.sh"
if grep -q autoscalar "${typo}/scope.sh" && ! printf 'test/e2e/autoscaler.sh\n' | sh "${typo}/scope.sh" > /dev/null 2>&1; then :; else
    echo 'FAIL: scope.sh accepts a rule that names a test the nightly run lacks'
    failures=$((failures + 1))
fi
rm -rf "${typo}"

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
if printf '%s' "${all}" | jq -e 'index("appliance-upgrade") == null' > /dev/null; then :; else
    echo 'FAIL: --all lists appliance-upgrade'
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
