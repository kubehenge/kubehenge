#!/bin/sh
# Keeps the changelog. Each pull request adds its lines as a fragment of its own in docs/changes/,
# so two pull requests never edit the same lines, and a release folds them into its section of
# docs/changelog.md. The rules for both are in .github/CONTRIBUTING.md.
#
#   scripts/changelog.sh check                     # refuses a fragment or a section out of shape
#   scripts/changelog.sh fold <version> [<date>]   # new section under [Unreleased], fragments deleted
#   scripts/changelog.sh unreleased                # the fragments' lines by heading, for the website
#   scripts/changelog.sh notes <version>           # the version's release notes, for its GitHub release
#
# Run it from anywhere in the repository. The date defaults to today. Lines fold in the order their
# fragments were first committed, and fragments git has not seen yet follow by name.
set -eu

usage="usage: $0 check | fold <version> [<date>] | unreleased | notes <version>"
mode=${1:?${usage}}
changes=docs/changes
changelog=docs/changelog.md
# Where a release page sends a link to a docs page: kubehenge.dev serves docs/<page>.md at
# /docs/<page>. A relative link would point into the release page's own address, where nothing is.
site=https://kubehenge.dev/docs/
# A section's headings, in the order they appear. A fragment takes them all but Highlights, which the
# release writes, and adds Internal, which the notes leave out.
order='Before you upgrade|Highlights|Added|Changed|Fixed|Removed|Security'
kinds='Before you upgrade|Added|Changed|Fixed|Removed|Security|Internal'
marker='<!-- Write one to five highlights here as bullets, then delete this line. -->'

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

# What a bullet must be, in both checks: one line of one or two sentences that ends in a full stop,
# at most 200 characters as read. A link counts by its text and code by what is inside the
# backticks. Under LC_ALL=C every awk counts bytes, so UTF-8's continuation bytes are dropped first.
# shellcheck disable=SC2016 # awk code, which awk expands
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
    if (length(read) > 200) refuse("a bullet over 200 characters (" length(read) "): " text)
    plain = text
    gsub(/`[^`]*`/, "code", plain)
    gsub(/\]\([^)]*\)/, "]", plain)
    if (plain !~ /\.$/) refuse("a bullet that does not end in a full stop: " text)
    else if (gsub(/[.!?][")]*[ \t]/, "&", plain) > 1) refuse("a bullet of more than two sentences: " text)
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
# each version holds its headings in order, Highlights among them, each with bullets. A highlight
# sums a change up in its own words, so one that repeats a bullet of its section is refused.
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
        function done(   text) {
            if (version == "" || unreleased) return
            empty()
            for (text in said) if (text in other) {
                printf "%s:%d: a highlight that repeats the bullet on line %d: %s\n", FILENAME, said[text], other[text], text > "/dev/stderr"
                bad = 1
            }
            if (!highlights) { printf "%s: %s has no ### Highlights with bullets\n", FILENAME, version > "/dev/stderr"; bad = 1 }
            if (highlights > 5) { printf "%s: %s has %d highlights, five at most\n", FILENAME, version, highlights > "/dev/stderr"; bad = 1 }
        }
        /^## / {
            done()
            version = $0
            unreleased = $0 == "## [Unreleased]"
            heading = ""
            last = bullets = highlights = 0
            split("", said)
            split("", other)
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
            if (heading == "Highlights") {
                highlights++
                said[substr($0, 3)] = FNR
            } else other[substr($0, 3)] = FNR
            bullet(substr($0, 3))
            next
        }
        { refuse("not a heading, a bullet or a blank line: " $0) }
        END { done(); exit bad }
    ' "${changelog}"
}

# anchors: every target a changelog link may name, one per line: each page of docs/ as page.md, and
# each of its headings as page.md#anchor. The anchor is GitHub's, which the website makes too
# (website/.vitepress/config.mts): link text kept, lower case, punctuation dropped, each space a
# hyphen, and a repeated one numbered from -1.
anchors() {
    for f in docs/*.md; do
        [ -e "${f}" ] || continue
        # shellcheck disable=SC2016 # awk code, which awk expands
        LC_ALL=C awk -v page="${f#docs/}" '
            BEGIN { print page }
            /^(```|~~~)/ { fence = !fence; next }
            fence || !/^#+ / { next }
            {
                text = $0
                sub(/^#+ +/, "", text)
                sub(/ +#+ *$/, "", text)
                plain = ""
                while (match(text, /\[[^]]*\]\([^)]*\)/)) {
                    link = substr(text, RSTART, RLENGTH)
                    plain = plain substr(text, 1, RSTART - 1) substr(link, 2, index(link, "](") - 2)
                    text = substr(text, RSTART + RLENGTH)
                }
                text = tolower(plain text)
                # U+2000 to U+203F, the punctuation block: dashes, curly quotes and the ellipsis.
                gsub(/\342\200[\200-\277]/, "", text)
                gsub(/[^a-z0-9_ \200-\377-]/, "", text)
                gsub(/ /, "-", text)
                if (seen[text]++) text = text "-" (seen[text] - 1)
                print page "#" text
            }
        ' "${f}"
    done
}

# check_links <file>...: refuses a link that is neither a web address nor a page of docs/ written
# relative to docs/changelog.md, or that names an anchor no heading of its page makes.
check_links() {
    anchors > "${work}/anchors"
    LC_ALL=C awk "${rules}"'
        NR == FNR { known[$0] = 1; next }
        {
            line = $0
            while (match(line, /\]\([^)]*\)/)) {
                target = substr(line, RSTART + 2, RLENGTH - 3)
                line = substr(line, RSTART + RLENGTH)
                if (target ~ /^[a-z]+:/) continue
                page = target
                sub(/#.*/, "", page)
                if (page !~ /^[A-Za-z0-9._-]+\.md$/) refuse("a link to " target ", which is neither a web address nor a page in docs/")
                else if (!(page in known)) refuse("a link to " page ", which is not a page in docs/")
                else if (!(target in known)) refuse("a link to " target ", an anchor no heading of " page " makes")
            }
        }
        END { exit bad }
    ' "${work}/anchors" "$@"
}

check() {
    bad=0
    for f in "${changes}"/*.md; do
        [ -e "${f}" ] || continue
        # shellcheck disable=SC2310 # its status is the verdict, and every file is checked before failing
        check_fragment "${f}" || bad=1
        # shellcheck disable=SC2310 # as for the fragment's shape
        check_links "${f}" || bad=1
    done
    # shellcheck disable=SC2310 # its status is the verdict, as for the fragments
    check_changelog || bad=1
    # shellcheck disable=SC2310 # as for the changelog's shape
    check_links "${changelog}" || bad=1
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

# notes <version>: the version's section without its heading, as its GitHub release shows it. Before
# 1.0.0 it opens with README's alpha warning, so nobody reads a release page without it. Each link to
# a docs page goes to the page on the website. The heading is matched as a plain string, since a
# bracket in an awk regex reads differently from one awk to the next.
notes() {
    # shellcheck disable=SC2016 # awk code, which awk expands
    LC_ALL=C awk -v head="## [$1]" -v site="${site}" '
        function rewrite(line,   out, target, page, anchor) {
            out = ""
            while (match(line, /\]\([A-Za-z0-9._-]+\.md(#[^)]*)?\)/)) {
                target = substr(line, RSTART + 2, RLENGTH - 3)
                page = target
                anchor = ""
                if (index(target, "#")) {
                    page = substr(target, 1, index(target, "#") - 1)
                    anchor = substr(target, index(target, "#"))
                }
                sub(/\.md$/, "", page)
                out = out substr(line, 1, RSTART - 1) "](" site page anchor ")"
                line = substr(line, RSTART + RLENGTH)
            }
            return out line
        }
        substr($0, 1, 4) == "## [" {
            on = substr($0, 1, length(head)) == head && (length($0) == length(head) || substr($0, length(head) + 1, 1) == " ")
            found = found || on
            next
        }
        on { print rewrite($0) }
        END { exit !found }
    ' "${changelog}" > "${work}/notes" || fail "${changelog} has no section for $1"
    case $1 in
        0.*)
            alpha=$(grep -m 1 '^> \*\*Alpha' README.md) || fail "README.md has no alpha warning"
            printf '%s\n' "${alpha}"
            ;;
        *) ;;
    esac
    cat "${work}/notes"
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
    fold | notes) ;;
    *) fail "${usage}" ;;
esac

version=${2:?${usage}}
case ${version} in
    *[!0-9.]* | '' | .* | *. | *..*) fail "the version ${version} is not numbers and dots, such as 0.4.3" ;;
    *) ;;
esac
if [ "${mode}" = notes ]; then
    notes "${version}"
    exit 0
fi
date=${3:-$(date +%F)}
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
