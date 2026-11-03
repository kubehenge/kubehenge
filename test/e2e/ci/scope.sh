#!/bin/sh
# Prints, as a JSON list, the end-to-end tests a merge group needs, from the paths its pull requests
# change, one per line on standard input:
#
#   git diff --name-only <base> HEAD | test/e2e/ci/scope.sh [--base <base>]
#   test/e2e/ci/scope.sh --all
#
# A path no rule below names needs them all, as --all does. Only the merge queue runs end-to-end
# tests, so a rule that leaves a test out lets a change reach main without it until the nightly run,
# which runs them all: leave a test out only for a path it cannot reach. The nightly run's own four
# are long, so a merge group runs one only for its own script and the code only it exercises.
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

workload='kubeadm rke2-etcd rke2-machines k3s-etcd k3s-machines talos kubeadm-addons-a kubeadm-addons-b rke2-addons-a rke2-addons-b rke2-addons-c k3s-addons-a k3s-addons-b talos-addons-a talos-addons-b'
all="management-cluster appliance ${workload} cluster-mesh node-image-refresh autoscaler"
# Tests the nightly run adds to --all.
nightly='management-cluster-upgrade rancher control-plane-ha two-nodes'
need=
[ "${1:-}" != --all ] || need=${all}
[ -n "${need}" ] || while IFS= read -r path; do
    case "${path}" in
        # Documentation, the other workflows and the checks' own settings run no end-to-end test; nor
        # do unit tests, nor the nightly run's own list.
        *.md | docs/* | website/* | LICENSE | .gitignore | .gitattributes | renovate.json | .yamllint.yaml) ;;
        .golangci.yml | .markdownlint* | .github/ISSUE_TEMPLATE/* | .github/workflows/ci.yml) ;;
        .github/workflows/codeql.yml | .github/workflows/scorecard.yml | .github/workflows/release.yml) ;;
        .github/workflows/release-build.yml | .github/workflows/fuzz.yml | scripts/sbom.sh | scripts/syft.yaml) ;;
        .github/codeql-config.yml | .github/zizmor.yml) ;;
        .github/workflows/website.yml | osv-scanner.toml | scripts/lint/*) ;;
        .github/workflows/cache.yml | scripts/cache-prune.sh) ;;
        appliance/* | pkg/appliance/* | pkg/ui/* | web/* | test/e2e/appliance.sh) need="${need} appliance" ;;
        *e2e_test.go) need="${need} management-cluster" ;;
        # They hold the Rancher and Turtles pins to what Rancher's test proved.
        pkg/provisioner/rancher*_test.go | pkg/provisioner/turtles*_test.go) need="${need} rancher" ;;
        *_test.go | */testdata/* | test/integration/*) ;;
        test/e2e/ci/nightly.sh) ;;
        test/e2e/management-cluster-upgrade.sh) need="${need} management-cluster-upgrade" ;;
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
        charts/kubehenge/files/kube-vip-credentials.sh) need="${need} ${all} control-plane-ha" ;;
        *) need="${need} ${all}" ;;
    esac
done

printf '['
sep=
for t in ${all} ${nightly}; do
    case " ${need} " in
        *" ${t} "*)
            printf '%s"%s"' "${sep}" "${t}"
            sep=', '
            ;;
        *) ;;
    esac
done
printf ']\n'
