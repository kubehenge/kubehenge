#!/bin/sh
# Every cosign verify of the build, the CI and the node images reads a trusted root fetched once,
# since a verify that fetches its own fails on one dropped connection to Sigstore's TUF server. A
# command continued over several lines is read as one.
#
#   test/unit/cosign-trusted-root.sh
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "${root}"

found=0 bad=0
for file in .github/workflows/*.yml scripts/*.sh pkg/templates/*/*.sh appliance/*.sh test/e2e/*.sh test/e2e/ci/*.sh; do
    [ -f "${file}" ] || continue
    commands=$(awk '
        { line = acc $0; acc = "" }
        /\\$/ { acc = substr(line, 1, length(line) - 1); next }
        line ~ /^[[:space:]]*#/ { next }
        line ~ /cosign"? verify / { print line }' "${file}")
    [ -n "${commands}" ] || continue
    while read -r command; do
        found=$((found + 1))
        case ${command} in
            *--trusted-root*) ;;
            *)
                printf 'FAIL: %s runs a cosign verify with no --trusted-root: %s\n' "${file}" "${command}"
                bad=$((bad + 1))
                ;;
        esac
    done << EOF
${commands}
EOF
done

# The release workflow fetches its root with the scripts' helper.
# shellcheck disable=SC2016 # the workflow's own text, which the runner's shell expands.
grep -q 'scripts/sigstore-root.sh "$RUNNER_TEMP/trusted_root.json"' .github/workflows/release.yml || {
    echo "FAIL: .github/workflows/release.yml does not fetch its trusted root with scripts/sigstore-root.sh"
    bad=$((bad + 1))
}

printf '%d cosign verify commands, %d failed\n' "${found}" "${bad}"
[ "${found}" -gt 0 ] && [ "${bad}" -eq 0 ]
