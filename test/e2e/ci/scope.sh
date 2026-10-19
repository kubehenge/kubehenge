#!/bin/sh
# Prints, as a JSON list, the end-to-end tests a pull request needs, from the paths it changes, one
# per line on standard input:
#
#   git diff --name-only <base> <head> | test/e2e/ci/scope.sh
#   test/e2e/ci/scope.sh --all
#
# A path no rule below names needs them all, as --all does. The merge queue runs them all whatever a pull request
# changes, so these rules decide only how soon a pull request hears of a failure, never what reaches
# main untested.
set -eu

all='management-cluster appliance kubeadm rke2 k3s cluster-mesh'
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
        .github/workflows/website.yml | osv-scanner.toml) ;;
        appliance/* | pkg/appliance/* | pkg/ui/* | web/* | test/e2e/appliance.sh) need="${need} appliance" ;;
        *e2e_test.go) need="${need} management-cluster" ;;
        *_test.go | */testdata/* | test/integration/*) ;;
        test/e2e/management-cluster-upgrade.sh | test/e2e/rancher.sh) ;;
        test/e2e/workload-cluster.sh) need="${need} ${workload}" ;;
        test/e2e/cluster-mesh.sh) need="${need} cluster-mesh" ;;
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
