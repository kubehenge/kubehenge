#!/bin/sh
# Keeps the changelog. Each pull request adds its lines as a fragment of its own in docs/changes/,
# so two pull requests never edit the same lines, and a release folds them into its section of
# docs/changelog.md. The rules for both are in .github/CONTRIBUTING.md.
#
#   scripts/changelog.sh check                     # refuses a fragment or a section out of shape
#   scripts/changelog.sh fold <version> [<date>]   # new section under [Unreleased], fragments deleted
#   scripts/changelog.sh unreleased                # the fragments' lines by heading, for the website
#
# Run it from anywhere in the repository. The date defaults to today. Lines fold in the order their
# fragments were first committed, and fragments git has not seen yet follow by name.
set -eu

usage="usage: $0 check | fold <version> [<date>] | unreleased"
mode=${1:?${usage}}
changes=docs/changes
changelog=docs/changelog.md
# A section's headings, in the order they appear. A fragment takes them all but Highlights, which the
# release writes, and adds Internal, which the notes leave out.
order='Before you upgrade|Highlights|Added|Changed|Fixed|Removed|Security'
kinds='Before you upgrade|Added|Changed|Fixed|Removed|Security|Internal'
marker='<!-- Write three to five highlights here as bullets, then delete this line. -->'

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

# What a bullet must be, in both checks: one line holding one sentence that ends in a full stop, of
# at most 140 characters as read. A link counts by its text and code by what is inside the
# backticks. Under LC_ALL=C every awk counts bytes, so UTF-8's continuation bytes are dropped first.
rules='
function refuse(why) {
    printf "%s:%d: %s\n", FILENAME, FNR, why > "/dev/stderr"
    bad = 1
}
function bullet(text,   read, plain) {
    read = text
    gsub(/\]\([^)]*\)/, "", read)
    gsub(/[[`]/, "", read)
    gsub(/[\200-\277]/, "", read)
    if (length(read) > 140) refuse("a bullet over 140 characters (" length(read) "): " text)
    plain = text
    gsub(/`[^`]*`/, "code", plain)
    gsub(/\]\([^)]*\)/, "]", plain)
    if (plain !~ /\.$/) refuse("a bullet that does not end in a full stop: " text)
    else if (plain ~ /[.!?][")]*[ \t]/) refuse("a bullet of more than one sentence: " text)
}
'

# check_fragment <file>: refuses a fragment out of shape, naming the file and line.
check_fragment() {
    if [ ! -s "$1" ]; then
        printf '%s: no bullet\n' "$1" >&2
        return 1
    fi
    LC_ALL=C awk -v kinds="${kinds}" "${rules}"'
        BEGIN { n = split(kinds, k, "|"); for (i = 1; i <= n; i++) kind[k[i]] = 1 }
        /^[ \t]*$/ { next }
        /^#/ {
            heading = $0
            sub(/^### /, "", heading)
            if ($0 !~ /^### / || !(heading in kind)) refuse("an unknown heading (### and one of " kinds "): " $0)
            next
        }
        /^- / {
            if (heading == "") refuse("a bullet before any heading: " $0)
            bullets++
            bullet(substr($0, 3))
            next
        }
        { refuse("not a heading, a bullet or a blank line: " $0) }
        END {
            if (!bullets) { printf "%s: no bullet\n", FILENAME > "/dev/stderr"; bad = 1 }
            exit bad
        }
    ' "$1"
}

# check_changelog: refuses a section of docs/changelog.md out of shape. [Unreleased] stays empty, and
# each version holds its headings in order, Highlights among them, each with bullets.
check_changelog() {
    [ -f "${changelog}" ] || fail "${changelog} is missing"
    grep -q '^## \[Unreleased\]$' "${changelog}" || fail "${changelog} has no '## [Unreleased]' heading"
    LC_ALL=C awk -v order="${order}" "${rules}"'
        BEGIN { n = split(order, o, "|"); for (i = 1; i <= n; i++) rank[o[i]] = i }
        function empty() {
            if (heading == "" || bullets) return
            printf "%s:%d: ### %s has no bullet\n", FILENAME, at, heading > "/dev/stderr"
            bad = 1
        }
        function done() {
            if (version == "" || unreleased) return
            empty()
            if (!highlights) { printf "%s: %s has no ### Highlights with bullets\n", FILENAME, version > "/dev/stderr"; bad = 1 }
            if (highlights > 5) { printf "%s: %s has %d highlights, five at most\n", FILENAME, version, highlights > "/dev/stderr"; bad = 1 }
        }
        /^## / {
            done()
            version = $0
            unreleased = $0 == "## [Unreleased]"
            heading = ""
            last = bullets = highlights = 0
            next
        }
        version == "" || /^[ \t]*$/ { next }
        unreleased { refuse("[Unreleased] stays empty, its lines are fragments in docs/changes/: " $0); next }
        /^<!--/ { refuse("the highlights are not written yet: " $0); next }
        /^#/ {
            name = $0
            sub(/^### /, "", name)
            if ($0 !~ /^### / || !(name in rank)) refuse("an unknown heading (### and one of " order "): " $0)
            else if (rank[name] <= last) refuse("a heading out of order (" order "): " $0)
            else {
                empty()
                heading = name
                last = rank[name]
                at = FNR
                bullets = 0
            }
            next
        }
        /^- / {
            if (heading == "") refuse("a bullet before any heading: " $0)
            bullets++
            if (heading == "Highlights") highlights++
            bullet(substr($0, 3))
            next
        }
        { refuse("not a heading, a bullet or a blank line: " $0) }
        END { done(); exit bad }
    ' "${changelog}"
}

check() {
    bad=0
    for f in "${changes}"/*.md; do
        [ -e "${f}" ] || continue
        check_fragment "${f}" || bad=1
    done
    check_changelog || bad=1
    [ "${bad}" = 0 ] || fail "out of shape: see the lines above and the rules in .github/CONTRIBUTING.md"
}

# grouped <dir>: the fragments' bullets merged under each heading, a file in <dir> for each heading.
grouped() {
    mkdir -p "$1"
    fragments | while IFS= read -r f; do
        awk -v dir="$1" '/^### / { heading = substr($0, 5); next } /^- / { print >> (dir "/" heading) }' "${f}"
    done
}

# body <dir> [highlights]: each heading that has lines, in order, and Highlights with the marker when
# asked.
body() {
    printf '%s\n' "${order}" | tr '|' '\n' | while IFS= read -r heading; do
        if [ "${heading}" = Highlights ]; then
            [ -z "${2:-}" ] || printf '\n### Highlights\n\n%s\n' "${marker}"
        elif [ -s "$1/${heading}" ]; then
            printf '\n### %s\n\n' "${heading}"
            cat "$1/${heading}"
        fi
    done
}

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

case ${mode} in
    check)
        check
        exit 0
        ;;
    unreleased)
        grouped "${work}/lines"
        body "${work}/lines"
        exit 0
        ;;
    fold) ;;
    *) fail "${usage}" ;;
esac

version=${2:?${usage}}
date=${3:-$(date +%F)}
case ${version} in
    *[!0-9.]* | '' | .* | *. | *..*) fail "the version ${version} is not numbers and dots, such as 0.4.3" ;;
    *) ;;
esac
case ${date} in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
    *) fail "the date ${date} is not YYYY-MM-DD" ;;
esac

check
if grep -q "^## \\[${version}\\]" "${changelog}"; then
    fail "${changelog} has a section for ${version} already"
fi
grouped "${work}/lines"
rm -f "${work}/lines/Internal"
body "${work}/lines" > "${work}/body"
[ -s "${work}/body" ] || fail "no changelog lines in ${changes} outside Internal"
{
    printf '## [%s] - %s\n' "${version}" "${date}"
    body "${work}/lines" highlights
} > "${work}/section"

awk -v section="${work}/section" '
    { print }
    /^## \[Unreleased\]$/ {
        print ""
        while ((getline line < section) > 0) print line
    }
' "${changelog}" > "${work}/changelog.md"
cat "${work}/changelog.md" > "${changelog}"
fragments | while IFS= read -r f; do rm -f "${f}"; done
printf 'changelog: %s folded into %s, write its highlights next\n' "${changes}" "${version}"
