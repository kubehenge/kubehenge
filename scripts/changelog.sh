#!/bin/sh
# Folds the changelog fragments of docs/changes/ into a release's section of docs/changelog.md, or
# prints the unreleased lines. A pull request adds its lines as a file of its own, so two pull
# requests never edit the same lines of the changelog.
#
#   scripts/changelog.sh <version> [<date>]   # new section under [Unreleased], fragments deleted
#   scripts/changelog.sh --unreleased         # the fragments' lines, in the order they fold in
#
# Run it from anywhere in the repository. The date defaults to today. A fragment holds bullet lines
# ("- ") and blank lines only. The lines fold in the order their fragments were first committed, and
# fragments git has not seen yet follow by name.
set -eu

usage="usage: $0 <version> [<date>] | --unreleased"
mode=${1:?${usage}}
changes=docs/changes
changelog=docs/changelog.md

fail() {
    printf 'changelog: %s\n' "$*" >&2
    exit 1
}

top=$(git rev-parse --show-toplevel)
cd "${top}"

# The fragments, one path per line, in fold order. A path that git log names but that is gone (folded
# by an earlier release) is skipped, and one added twice is listed once.
fragments() {
    {
        git log --topo-order --reverse --diff-filter=A --name-only --format= -- "${changes}" 2> /dev/null || true
        for f in "${changes}"/*.md; do
            [ -e "${f}" ] && printf '%s\n' "${f}"
        done
    } | while IFS= read -r f; do
        case ${f} in
            "${changes}"/*.md) [ -f "${f}" ] && printf '%s\n' "${f}" ;;
            *) ;;
        esac
    done | awk '!seen[$0]++'
}

# The lines of every fragment, each checked to be a bullet.
lines() {
    fragments | while IFS= read -r f; do
        n=0
        while IFS= read -r line || [ -n "${line}" ]; do
            n=$((n + 1))
            case ${line} in
                '') ;;
                '- '*) printf '%s\n' "${line}" ;;
                *) fail "${f}:${n}: not a bullet line (every line starts with '- '): ${line}" ;;
            esac
        done < "${f}"
    done
}

if [ "${mode}" = --unreleased ]; then
    lines
    exit 0
fi

version=${mode}
date=${2:-$(date +%F)}
case ${version} in
    *[!0-9.]* | '' | .* | *. | *..*) fail "the version ${version} is not numbers and dots, such as 0.4.3" ;;
    *) ;;
esac
case ${date} in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
    *) fail "the date ${date} is not YYYY-MM-DD" ;;
esac

[ -f "${changelog}" ] || fail "${changelog} is missing"
grep -q '^## \[Unreleased\]$' "${changelog}" || fail "${changelog} has no '## [Unreleased]' heading"
if grep -q "^## \\[${version}\\]" "${changelog}"; then
    fail "${changelog} has a section for ${version} already"
fi

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT
# set -e ends the script when a fragment is refused, which a pipe into awk would not.
lines > "${work}/lines"
[ -s "${work}/lines" ] || fail "no changelog lines in ${changes}"

awk -v heading="## [${version}] - ${date}" -v lines="${work}/lines" '
    { print }
    /^## \[Unreleased\]$/ {
        print ""
        print heading
        print ""
        while ((getline line < lines) > 0) print line
    }
' "${changelog}" > "${work}/changelog.md"
cat "${work}/changelog.md" > "${changelog}"
fragments | while IFS= read -r f; do rm -f "${f}"; done
printf 'changelog: %s folded into %s\n' "${changes}" "${version}"
