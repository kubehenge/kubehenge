#!/bin/sh
# Unit test of scripts/changelog.sh: its check of the fragments in docs/changes/ and of the sections
# of docs/changelog.md, and its fold of the fragments into a release's section. A repository is made
# for each case, with fragments committed in an order that differs from their names'. Needs no
# Proxmox VE.
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

# A released section in the shape the check wants.
released='## [0.4.2] - 2026-11-05

### Highlights

- Old highlight.

### Fixed

- Old line.'

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

# repo <name> [<sections>]: a repository with the script and a changelog holding <sections> below
# [Unreleased], one released section by default.
repo() {
    dir=${root}/$1
    mkdir -p "${dir}/scripts" "${dir}/docs/changes"
    cp "${script}" "${dir}/scripts/changelog.sh"
    printf '# Changelog\n\n## [Unreleased]\n\n%s\n' "${2:-${released}}" > "${dir}/docs/changelog.md"
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
# run <repo> <args>...: the script's output and exit status, in out and status.
run() {
    name=$1
    shift
    status=0
    out=$(cd "${root}/${name}" && sh scripts/changelog.sh "$@" 2>&1) || status=$?
}
# refused <case> <fragment text> <message>: check refuses the fragment and names its file.
refused() {
    repo "$1"
    fragment "$1" bad "$2"
    run "$1" check
    same "$1: refused" 1 "${status}"
    mentions "$1: says why" "docs/changes/bad.md" "${out}"
    mentions "$1: says why" "$3" "${out}"
}
# section_refused <case> <section> <message>: check refuses the changelog with the section.
section_refused() {
    repo "$1" "$2"
    run "$1" check
    same "$1: refused" 1 "${status}"
    mentions "$1: says why" "$3" "${out}"
}

long=$(printf '%0141d' 0 | tr 0 a)
accented=$(printf '%0139d' 0 | sed 's/0/é/g')

# A valid fragment: every heading a fragment may hold, a link counted by its text, code by what
# is inside the backticks, and 140 letters with accents counted as letters, not bytes.
repo valid
fragment valid good "### Before you upgrade

- Upgrade the appliance before its clusters.

### Added

- A \`--vlan\` option, described in [install.md](install.md#a-very-long-anchor-that-would-take-the-line-past-the-limit-if-it-counted).
- ${accented}.

### Changed

- Workers get 6144 MiB, up from 4096 MiB, so 1.0.0 stays one word.

### Fixed

- Nodes keep their identity.

### Removed

- The \`--old\` option.

### Security

- The API token is never logged.

### Internal

- CI runs the shell tests."
run valid check
same 'valid fragment: passes' 0 "${status}"
same 'valid fragment: says nothing' '' "${out}"

# Each refusal of a fragment.
refused unknown-heading '### Misc

- A line.' 'an unknown heading'
refused wrong-level '## Added

- A line.' 'an unknown heading'
refused missing-heading '- A line without a heading.' 'a bullet before any heading'
refused too-long "### Fixed

- ${long}." 'a bullet over 140 characters (142)'
refused two-sentences '### Fixed

- Nodes keep their identity. A reboot keeps it too.' 'a bullet of more than one sentence'
refused no-full-stop '### Fixed

- Nodes keep their identity' 'does not end in a full stop'
refused prose '### Fixed

A line without a bullet.' 'not a heading, a bullet or a blank line'
refused nested '### Fixed

- A line.
  - A line under it.' 'not a heading, a bullet or a blank line'
refused no-bullet '### Fixed' 'no bullet'
refused empty '' 'no bullet'

# Each refusal of a released section.
section_refused out-of-order '## [0.4.2] - 2026-11-05

### Highlights

- A highlight.

### Fixed

- A fix.

### Added

- A feature.' 'a heading out of order'
section_refused highlights-not-first '## [0.4.2] - 2026-11-05

### Added

- A feature.

### Highlights

- A highlight.' 'a heading out of order'
section_refused twice '## [0.4.2] - 2026-11-05

### Highlights

- A highlight.

### Fixed

- A fix.

### Fixed

- Another fix.' 'a heading out of order'
section_refused internal '## [0.4.2] - 2026-11-05

### Highlights

- A highlight.

### Internal

- CI runs the shell tests.' 'an unknown heading'
section_refused no-highlights '## [0.4.2] - 2026-11-05

### Fixed

- A fix.' 'has no ### Highlights'
section_refused six-highlights '## [0.4.2] - 2026-11-05

### Highlights

- One.
- Two.
- Three.
- Four.
- Five.
- Six.' 'has 6 highlights, five at most'
section_refused marker '## [0.4.2] - 2026-11-05

### Highlights

<!-- Write one to five highlights here as bullets, then delete this line. -->

### Fixed

- A fix.' 'the highlights are not written yet'
section_refused empty-heading '## [0.4.2] - 2026-11-05

### Highlights

- A highlight.

### Fixed

### Security

- A hardening.' '### Fixed has no bullet'
section_refused section-bullet "## [0.4.2] - 2026-11-05

### Highlights

- ${long}." 'a bullet over 140 characters'
repo unreleased-lines
printf '# Changelog\n\n## [Unreleased]\n\n- A line.\n\n%s\n' "${released}" > "${root}/unreleased-lines/docs/changelog.md"
run unreleased-lines check
same 'unreleased lines: refused' 1 "${status}"
mentions 'unreleased lines: says why' '[Unreleased] stays empty' "${out}"

# The fold: each heading's bullets from every fragment in commit order, Highlights left to write,
# Internal left out and the fragments deleted.
repo several
fragment several zebra '### Added

- First committed.

### Internal

- CI runs faster.'
fragment several apple '### Fixed

- Second committed.

### Before you upgrade

- Read this first.'
fragment several mango '### Added

- Third committed.'
run several unreleased
same 'unreleased: grouped, without Highlights or Internal' '
### Before you upgrade

- Read this first.

### Added

- First committed.
- Third committed.

### Fixed

- Second committed.' "${out}"
files_in several
same 'unreleased: leaves the fragments' 3 "${count}"
# A fragment git has not seen follows the committed ones.
printf '### Fixed\n\n- Not committed.\n' > "${root}/several/docs/changes/aaa.md"
run several fold 0.5.0 2026-11-07
same 'fold: succeeds' 0 "${status}"
section_of several 0.5.0 0.4.2
same 'fold: section in order' '## [0.5.0] - 2026-11-07

### Before you upgrade

- Read this first.

### Highlights

<!-- Write one to five highlights here as bullets, then delete this line. -->

### Added

- First committed.
- Third committed.

### Fixed

- Second committed.
- Not committed.

## [0.4.2] - 2026-11-05' "${text}"
files_in several
same 'fold: fragments deleted' 0 "${count}"
run several check
same 'fold: check refuses the unwritten highlights' 1 "${status}"
mentions 'fold: names the highlights' 'the highlights are not written yet' "${out}"

# A fragment out of shape stops the fold, and nothing changes.
repo stopped
fragment stopped good '### Fixed

- A good line.'
fragment stopped bad 'A line without a bullet.'
changelog_of stopped
before=${text}
run stopped fold 0.4.3 2026-11-06
same 'bad fragment: fold refused' 1 "${status}"
mentions 'bad fragment: names the file and line' 'docs/changes/bad.md:1: not a heading' "${out}"
changelog_of stopped
same 'bad fragment: changelog unchanged' "${before}" "${text}"
files_in stopped
same 'bad fragment: fragments stay' 2 "${count}"

# No fragments, or only internal lines: refused, and the changelog stays.
repo none
changelog_of none
before=${text}
run none fold 0.4.3 2026-11-06
same 'no fragments: refused' 1 "${status}"
mentions 'no fragments: says so' 'no changelog lines' "${out}"
changelog_of none
same 'no fragments: changelog unchanged' "${before}" "${text}"
fragment none ci '### Internal

- CI runs faster.'
run none fold 0.4.3 2026-11-06
same 'only internal: refused' 1 "${status}"
files_in none
same 'only internal: fragment stays' 1 "${count}"

# A version that has a section already, or is not numbers, is refused.
repo again
fragment again line '### Fixed

- A line.'
run again fold 0.4.2 2026-11-06
same 'existing version: refused' 1 "${status}"
mentions 'existing version: says so' 'section for 0.4.2 already' "${out}"
files_in again
same 'existing version: fragment stays' 1 "${count}"
run again fold v0.4.3 2026-11-06
same 'bad version: refused' 1 "${status}"
run again fold 0.4.3 06.11.2026
same 'bad date: refused' 1 "${status}"

if [ "${failures}" -gt 0 ]; then
    printf '%s failed\n' "${failures}"
    exit 1
fi
echo "changelog: ok (${passes} checks)"
