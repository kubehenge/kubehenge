#!/bin/sh
# Unit test of scripts/subjects.sh, which refuses a pull request title over 64 characters or a
# commit subject over 72. Needs no Proxmox VE.
#
#   test/unit/subjects.sh
set -eu

script=$(dirname "$0")/../../scripts/subjects.sh

passes=0 failures=0
# expect <what> <wanted status> <kind> <subject>
expect() {
    status=0
    out=$(printf '%s\n' "$4" | sh "${script}" "$3" 2>&1) || status=$?
    if [ "${status}" = "$2" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted status %s, got %s: %s\n' "$1" "$2" "${status}" "${out}"
        failures=$((failures + 1))
    fi
}

fits=$(printf '%072d' 0 | tr 0 a)
over=$(printf '%073d' 0 | tr 0 a)
title_fits=$(printf '%064d' 0 | tr 0 a)
title_over=$(printf '%065d' 0 | tr 0 a)
accented=$(printf '%072d' 0 | sed 's/0/é/g')
title_accented=$(printf '%064d' 0 | sed 's/0/é/g')

expect 'title of 64 characters: passes' 0 title "${title_fits}"
expect 'title of 65 characters: refused' 1 title "${title_over}"
expect 'title of 64 letters with accents: counted as letters' 0 title "${title_accented}"
expect 'commit subject of 72 characters: passes' 0 commits "${fits}"
expect 'commit subject of 73 characters: refused' 1 commits "${over}"
expect '72 letters with accents: counted as letters' 0 commits "${accented}"
expect 'several subjects, one long: refused' 1 commits "Short subject
${over}
Another short one"
expect 'long merge of a branch: passes' 0 commits "Merge branch 'a-branch-name-long-enough-to-take-the-subject-past-the-limit' into batch-3"
expect 'long merge of a remote-tracking branch: passes' 0 commits "Merge remote-tracking branch 'origin/a-branch-name-long-enough-to-pass-the-limit-of-72'"
expect 'long merge from another repository: passes' 0 commits "Merge branch 'main' of https://github.com/someone/some-repository-with-a-long-name into batch-3"
expect 'long merge subject in a title: refused' 1 title "Merge branch 'a-branch-name-long-enough-to-take-the-subject-past-the-limit' into batch-3"
expect 'long subject that only starts like a merge: refused' 1 commits "Merge branch 'main' and fix the installer's handling of storages with little free space"
expect 'unknown kind: refused' 2 subject 'A subject'

out=$(printf '%s\n' "${title_over}" | sh "${script}" title 2>&1) || true
case ${out} in
    *'the title is 65 characters, 64 at most'*) passes=$((passes + 1)) ;;
    *)
        printf 'FAIL: names the length\n  got: %s\n' "${out}"
        failures=$((failures + 1))
        ;;
esac

if [ "${failures}" -gt 0 ]; then
    printf '%s failed\n' "${failures}"
    exit 1
fi
echo "subjects: ok (${passes} checks)"
