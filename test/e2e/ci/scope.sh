#!/bin/sh
# Prints, as a JSON list of lanes, the end-to-end tests a pull request needs, from the paths it
# changes, one per line on standard input. A lane is a test, or two run one after the other
# on one runner, as in k3s-etcd,cluster-mesh:
#
#   git -c core.quotepath=off diff --name-only --no-renames <base> HEAD | test/e2e/ci/scope.sh [--base <base>]
#   test/e2e/ci/scope.sh --all
#
# A path no rule below names needs them all, as --all does. Only a pull request that is ready runs
# end-to-end tests, so a rule that leaves a test out lets a change reach main
# without it until the nightly run, which runs them all: leave a test out only for a path it cannot
# reach. The nightly run's own tests are long, so a pull request runs one only for its own script
# and the code only it exercises.
# With --base, run in the repository's checkout, a change to the chart's values brings Rancher's
# test only when it changes addons.rancher since <base>, a commit or tree; without it, any change to
# them does.
set -eu

base=
if [ "${1:-}" = --base ]; then
    base=${2:?usage: $0 [--base <commit>] | --all}
    shift 2
fi
# rancher_block <values.yaml>: the values' addons.rancher block, from its key to the next key or
# comment at its depth.
rancher_block() {
    awk '{ sub(/\r$/, "") } /^addons:/ { a = 1; next } /^[^ #]/ { a = 0 } a && /^  rancher:/ { r = 1; print; next }
        r && /^  [^ ]/ { r = 0 } r { print }'
}
# rancher_changed: whether addons.rancher differs between <base> and the checkout.
rancher_changed() {
    [ -n "${base}" ] || return 0
    root=$(git rev-parse --show-toplevel)
    before=$(git show "${base}:charts/kubehenge/values.yaml") || return 0
    before=$(printf '%s\n' "${before}" | rancher_block)
    after=$(rancher_block < "${root}/charts/kubehenge/values.yaml")
    [ "${before}" != "${after}" ]
}

kubeadm='kubeadm kubeadm-addons-a kubeadm-addons-b kubeadm-addons-c'
rke2='rke2-etcd rke2-machines rke2-addons-a rke2-addons-b rke2-addons-c rke2-addons-d'
k3s='k3s-etcd k3s-machines k3s-addons-a k3s-addons-b'
talos='talos talos-addons-a talos-addons-b talos-addons-c'
workload="${kubeadm} ${rke2} ${k3s} ${talos}"
all="management-cluster appliance ${workload} cluster-mesh node-image-refresh autoscaler"
# The tests that make a cluster of kubehenge-k3s themselves, beside the workload tests of k3s. The
# nightly run's own four do too, and join a pull request only for their own code.
k3s_users='cluster-mesh node-image-refresh autoscaler'
# The tests that run `kubehenge serve`: the appliance's service, and the one workload test that
# replaces machines in a namespace the interface creates in (workload-cluster.sh, creator_config),
# where `kubehenge cluster` acts through it.
interface_users='appliance k3s-machines'
# The tests that install the chart and look for its ClusterClasses (lib.sh, appliance.sh).
chart_users='management-cluster appliance'
# Tests the nightly run adds to --all.
nightly='management-cluster-upgrade rancher control-plane-ha two-nodes appliance-upgrade'
# Tests that share a lane, one after the other on its runner, which keeps a pull request at 20 lanes,
# as many as a run's jobs at once. Each pair ends within the longest lane, and the first test leaves
# no guest behind (docs/testing.md).
pairs='appliance,management-cluster k3s-etcd,cluster-mesh k3s-addons-b,autoscaler'
# lane_of <test>: sets lane to the lane the test runs in, its pair or itself. It sets a variable
# because a command substitution starts a process for each test.
lane_of() {
    lane=$1
    for lane_pair in ${pairs}; do
        case ",${lane_pair}," in
            *",$1,"*)
                lane=${lane_pair}
                return
                ;;
            *) ;;
        esac
    done
}
need=
[ "${1:-}" != --all ] || need=${all}
[ -n "${need}" ] || while IFS= read -r path; do
    case "${path}" in
        '') ;;
        # Documentation, the other workflows and the checks' own settings run no end-to-end test; nor
        # do unit tests, nor the nightly run's own list.
        *.md | docs/* | website/* | LICENSE | .gitignore | .gitattributes | renovate.json | .yamllint.yaml) ;;
        .golangci.yml | .markdownlint* | .github/ISSUE_TEMPLATE/* | .github/workflows/ci.yml) ;;
        .github/workflows/codeql.yml | .github/workflows/scorecard.yml | .github/workflows/release.yml) ;;
        .github/workflows/release-build.yml | .github/workflows/fuzz.yml | scripts/sbom.sh | scripts/syft.yaml | scripts/changelog.sh) ;;
        scripts/subjects.sh) ;;
        .github/codeql-config.yml | .github/zizmor.yml) ;;
        .github/workflows/website.yml | osv-scanner.toml | scripts/lint/*) ;;
        .github/workflows/cache.yml | scripts/cache-prune.sh) ;;
        .github/workflows/links.yml | .github/actionlint.yaml | .github/dependency-review-config.yml) ;;
        .typos.toml | lychee.toml) ;;
        # The web interface's unit tests, its lint settings and the script that takes the docs' screenshots.
        web/*.test.ts | web/*.test.tsx | web/screenshots.ts | web/stylelint.config.mjs | web/osv-scanner.toml) ;;
        # The Go tests that run against a Proxmox VE host are the tests' own, as `make e2e-*` runs them.
        pkg/appliance/e2e_test.go) need="${need} appliance" ;;
        *e2e_test.go) need="${need} management-cluster" ;;
        # They hold the Rancher and Turtles pins to what Rancher's test proved.
        pkg/provisioner/rancher*_test.go | pkg/provisioner/turtles*_test.go | charts/kubehenge/rancher_test.go)
            need="${need} rancher"
            ;;
        *_test.go) ;;
        # The interface, the appliance's commands and what installs the appliance. The interface is
        # also what `kubehenge cluster` reaches, in the one workload test that runs it.
        pkg/ui/* | pkg/cli/serve.go | pkg/cli/cluster.go) need="${need} ${interface_users}" ;;
        # Only the appliance's service and installer run the built web pages.
        appliance/* | pkg/appliance/* | web/* | test/e2e/appliance.sh) need="${need} appliance" ;;
        pkg/cli/appliance.go | pkg/cli/activity.go | pkg/cli/notifications.go | pkg/cli/update.go) need="${need} appliance" ;;
        */testdata/* | test/integration/*) ;;
        # The support bundle is made by every workload test, and by the interface for a cluster.
        pkg/diagnose/* | pkg/cli/diagnose.go) need="${need} appliance ${workload}" ;;
        # `etcd-snapshot decrypt` reads back the snapshots the workload tests take.
        pkg/cli/etcdsnapshot.go | test/e2e/cis-benchmark.sh) need="${need} ${workload}" ;;
        # A distribution's ClusterClass is used by the tests that make a cluster of it, and read by
        # the tests that install the chart. The helpers every class includes are in _helpers.tpl.
        charts/kubehenge/templates/kubeadm.yaml) need="${need} ${chart_users} ${kubeadm}" ;;
        charts/kubehenge/templates/rke2.yaml) need="${need} ${chart_users} ${rke2}" ;;
        charts/kubehenge/templates/k3s.yaml) need="${need} ${chart_users} ${k3s} ${k3s_users}" ;;
        charts/kubehenge/templates/talos.yaml) need="${need} ${chart_users} ${talos}" ;;
        # Only `make unit` runs the unit tests, and only this script and the nightly list choose lanes.
        test/e2e/ci/nightly.sh | test/e2e/ci/scope.sh | test/unit/*) ;;
        test/e2e/management-cluster-upgrade.sh) need="${need} management-cluster-upgrade" ;;
        test/e2e/appliance-upgrade.sh) need="${need} appliance-upgrade" ;;
        test/e2e/rancher.sh) need="${need} rancher" ;;
        test/e2e/control-plane-ha.sh) need="${need} control-plane-ha" ;;
        test/e2e/two-nodes.sh | test/e2e/ci/second-node.sh) need="${need} two-nodes" ;;
        test/e2e/workload-cluster.sh) need="${need} ${workload}" ;;
        test/e2e/backups.sh) need="${need} ${workload} cluster-mesh" ;;
        test/e2e/cluster-mesh.sh) need="${need} cluster-mesh" ;;
        test/e2e/node-image-refresh.sh) need="${need} node-image-refresh" ;;
        test/e2e/autoscaler.sh) need="${need} autoscaler" ;;
        test/e2e/management-cluster.sh | test/e2e/lib.sh) need="${need} management-cluster management-cluster-upgrade" ;;
        management-cluster/* | pkg/provisioner/operator.go | pkg/cli/wait.go | scripts/provider-image.sh | scripts/*-patches/*)
            need="${need} ${all} management-cluster-upgrade"
            ;;
        charts/kubehenge/values.yaml)
            need="${need} ${all}"
            # shellcheck disable=SC2310 # its answer is the condition.
            if rancher_changed; then need="${need} rancher"; fi
            ;;
        pkg/provisioner/rancher*.go | pkg/provisioner/turtles*.go | pkg/provisioner/hooks.go | pkg/provisioner/upgradehold.go) need="${need} ${all} rancher" ;;
        pkg/provisioner/placement.go | pkg/provisioner/ha*.go) need="${need} ${all} two-nodes" ;;
        # A release tag is accepted only on a commit whose pull request passed, and the release reruns no
        # lane, so the release pull request (appVersion) runs them all.
        charts/kubehenge/Chart.yaml) need="${need} ${all}" ;;
        charts/kubehenge/files/kube-vip-credentials.sh) need="${need} ${all} control-plane-ha" ;;
        *) need="${need} ${all}" ;;
    esac
done

# A rule that names a test the nightly run does not have would leave a lane out for good.
for t in ${need}; do
    case " ${all} ${nightly} " in
        *" ${t} "*) ;;
        *)
            echo "scope.sh: unknown test ${t}" >&2
            exit 1
            ;;
    esac
done

printf '['
sep=
printed=' '
for t in ${all} ${nightly}; do
    case " ${need} " in
        *" ${t} "*) ;;
        *) continue ;;
    esac
    lane_of "${t}"
    case "${printed}" in
        *" ${lane} "*) ;;
        *)
            printf '%s"%s"' "${sep}" "${lane}"
            sep=', '
            printed="${printed}${lane} "
            ;;
    esac
done
printf ']\n'
