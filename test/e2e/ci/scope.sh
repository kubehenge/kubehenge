#!/bin/sh
# Prints, as a JSON list, the end-to-end tests a merge group needs, from the paths its pull requests
# change, one per line on standard input:
#
#   git diff --name-only <base> <head> | test/e2e/ci/scope.sh
#   test/e2e/ci/scope.sh --all
#
# A path no rule below names needs them all, as --all does. Only the merge queue runs end-to-end
# tests, so a rule that leaves a test out lets a change reach main without it until the nightly run,
# which runs them all: leave a test out only for a path it cannot reach.
set -eu

all='management-cluster appliance kubeadm rke2 k3s cluster-mesh node-image-refresh autoscaler'
workload='kubeadm rke2 k3s'
need=
[ "${1:-}" != --all ] || need=${all}
[ -n "${need}" ] || while IFS= read -r path; do
    case "${path}" in
        # Documentation, the other workflows and the checks' own settings run no end-to-end test; nor
        # do unit tests, nor the nightly-only tests, which the nightly run covers.
        *.md | docs/* | website/* | LICENSE | .gitignore | .gitattributes | renovate.json | .yamllint.yaml) ;;
        .golangci.yml | .markdownlint* | .github/ISSUE_TEMPLATE/* | .github/workflows/ci.yml) ;;
        .github/workflows/codeql.yml | .github/workflows/scorecard.yml | .github/workflows/release.yml) ;;
        .github/workflows/release-build.yml | .github/workflows/fuzz.yml | scripts/sbom.sh | scripts/syft.yaml) ;;
        .github/codeql-config.yml | .github/zizmor.yml) ;;
        .github/workflows/website.yml | osv-scanner.toml | scripts/lint/*) ;;
        appliance/* | pkg/appliance/* | pkg/ui/* | web/* | test/e2e/appliance.sh) need="${need} appliance" ;;
        *e2e_test.go) need="${need} management-cluster" ;;
        *_test.go | */testdata/* | test/integration/*) ;;
        test/e2e/management-cluster-upgrade.sh | test/e2e/rancher.sh) ;;
        test/e2e/workload-cluster.sh) need="${need} ${workload}" ;;
        test/e2e/cluster-mesh.sh) need="${need} cluster-mesh" ;;
        test/e2e/node-image-refresh.sh) need="${need} node-image-refresh" ;;
        test/e2e/autoscaler.sh) need="${need} autoscaler" ;;
        test/e2e/management-cluster.sh | test/e2e/lib.sh) need="${need} management-cluster" ;;
        *) need="${need} ${all}" ;;
    esac
done

printf '['
sep=
for t in ${all}; do
    case " ${need} " in
        *" ${t} "*)
            printf '%s"%s"' "${sep}" "${t}"
            sep=', '
            ;;
        *) ;;
    esac
done
printf ']\n'
