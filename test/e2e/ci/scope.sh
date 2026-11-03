#!/bin/sh
# Prints, as a JSON list, the end-to-end tests a merge group needs, from the paths its pull requests
# change, one per line on standard input:
#
#   git diff --name-only <base> <head> | test/e2e/ci/scope.sh
#   test/e2e/ci/scope.sh --all
#
# A path no rule below names needs them all and the nightly run's four as well (--all lists only
# the first). Only the merge queue runs end-to-end
# tests, so a rule that leaves a test out lets a change reach main without it until the nightly run,
# which runs them all: leave a test out only for a path it cannot reach.
set -eu

workload='kubeadm rke2 k3s talos kubeadm-addons rke2-addons k3s-addons talos-addons'
all="management-cluster appliance ${workload} cluster-mesh node-image-refresh autoscaler"
# Tests the nightly run adds to --all. A merge group runs one only when a change reaches it.
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
        appliance/* | pkg/appliance/* | pkg/ui/* | web/* | test/e2e/appliance.sh) need="${need} appliance" ;;
        *e2e_test.go) need="${need} management-cluster" ;;
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
        *) need="${need} ${all} ${nightly}" ;;
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
