#!/bin/sh
# Unit test of scripts/changelog.sh, which folds the fragments of docs/changes/ into a release's
# section of docs/changelog.md: a repository made for each case, with fragments committed in an
# order that differs from their names'. Needs no Proxmox VE.
#
#   test/unit/changelog.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
script=${here}/../../scripts/changelog.sh
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# same <what> <wanted> <got>
same() {
    if [ "$2" = "$3" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}
# mentions <what> <needle> <text>
mentions() {
    case $3 in
        *"$2"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
            failures=$((failures + 1))
            ;;
    esac
}

# changelog_of <repo>: the repository's changelog, in text.
changelog_of() {
    text=$(cat "${root}/$1/docs/changelog.md")
}
# section_of <repo> <from> <to>: the changelog's lines from the heading <from> to the heading <to>.
section_of() {
    text=$(sed -n "/^## \\[$2\\]/,/^## \\[$3\\]/p" "${root}/$1/docs/changelog.md")
}
# files_in <repo>: how many fragments the repository has left, in count.
files_in() {
    count=0
    for f in "${root}/$1/docs/changes"/*; do
        [ -e "${f}" ] || continue
        count=$((count + 1))
    done
}

# repo <name>: a repository with a changelog that has one released section and the script.
repo() {
    dir=${root}/$1
    mkdir -p "${dir}/scripts" "${dir}/docs/changes"
    cp "${script}" "${dir}/scripts/changelog.sh"
    printf '# Changelog\n\n## [Unreleased]\n\n## [0.4.2] - 2026-11-05\n\n- Old line.\n' > "${dir}/docs/changelog.md"
    git -C "${dir}" init -q
    git -C "${dir}" config user.name test
    git -C "${dir}" config user.email test@example.invalid
    git -C "${dir}" config commit.gpgsign false
    git -C "${dir}" config core.hooksPath /dev/null
    git -C "${dir}" add .
    git -C "${dir}" commit -q -m base
}
# fragment <repo> <name> <text>: commits docs/changes/<name>.md.
fragment() {
    printf '%s\n' "$3" > "${root}/$1/docs/changes/$2.md"
    git -C "${root}/$1" add docs/changes
    git -C "${root}/$1" commit -q -m "$2"
}
# fold <repo> <args>...: the script's output and exit status, in out and status.
fold() {
    name=$1
    shift
    status=0
    out=$(cd "${root}/${name}" && sh scripts/changelog.sh "$@" 2>&1) || status=$?
}

# No fragments: refused, and the changelog stays.
repo none
changelog_of none
before=${text}
fold none 0.4.3 2026-11-06
same 'no fragments: refused' 1 "${status}"
mentions 'no fragments: says so' 'no changelog lines' "${out}"
changelog_of none
same 'no fragments: changelog unchanged' "${before}" "${text}"

# One fragment.
repo one
fragment one node-identity '- Nodes keep their identity.'
fold one 0.4.3 2026-11-06
same 'one fragment: succeeds' 0 "${status}"
changelog_of one
same 'one fragment: changelog' '# Changelog

## [Unreleased]

## [0.4.3] - 2026-11-06

- Nodes keep their identity.

## [0.4.2] - 2026-11-05

- Old line.' "${text}"
files_in one
same 'one fragment: fragment deleted' 0 "${count}"

# Several fragments fold in the order they were first committed, not the order of their names.
repo several
fragment several zebra '- First committed.
- Its second line.'
fragment several apple '- Second committed.'
fragment several mango '- Third committed.'
fold several --unreleased
same 'several: unreleased lines in commit order' '- First committed.
- Its second line.
- Second committed.
- Third committed.' "${out}"
files_in several
same 'several: --unreleased leaves the fragments' 3 "${count}"
# A fragment git has not seen follows the committed ones.
printf '%s\n' '- Not committed.' > "${root}/several/docs/changes/aaa.md"
fold several 0.5.0 2026-11-07
same 'several: succeeds' 0 "${status}"
section_of several 0.5.0 0.4.2
same 'several: section' '## [0.5.0] - 2026-11-07

- First committed.
- Its second line.
- Second committed.
- Third committed.
- Not committed.

## [0.4.2] - 2026-11-05' "${text}"
files_in several
same 'several: fragments deleted' 0 "${count}"

# A line that is not a bullet is refused with its place, and nothing changes.
repo prose
fragment prose good '- A good line.'
fragment prose bad 'A line without a bullet.'
changelog_of prose
before=${text}
fold prose 0.4.3 2026-11-06
same 'prose: refused' 1 "${status}"
mentions 'prose: names the file and line' 'docs/changes/bad.md:1: not a bullet line' "${out}"
changelog_of prose
same 'prose: changelog unchanged' "${before}" "${text}"
files_in prose
same 'prose: fragments stay' 2 "${count}"

# A version that has a section already is refused.
repo again
fragment again line '- A line.'
fold again 0.4.2 2026-11-06
same 'existing version: refused' 1 "${status}"
mentions 'existing version: says so' 'section for 0.4.2 already' "${out}"
files_in again
same 'existing version: fragment stays' 1 "${count}"

# A version that is not numbers is refused.
fold again v0.4.3 2026-11-06
same 'bad version: refused' 1 "${status}"

if [ "${failures}" -gt 0 ]; then
    printf '%s failed\n' "${failures}"
    exit 1
fi
echo "changelog: ok (${passes} checks)"
