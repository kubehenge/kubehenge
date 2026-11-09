#!/bin/sh
# Unit test of scripts/subjects.sh, which refuses a pull request title or commit subject over 72
# characters. Needs no Proxmox VE.
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

# letters <n>: n letters a.
letters() {
    printf "%0$1d" 0 | tr 0 a
}

expect '72 characters: passes' 0 title "$(letters 72)"
expect '73 characters: refused' 1 title "$(letters 73)"
expect '73 characters in a commit: refused' 1 commits "$(letters 73)"
expect '72 letters with accents: counted as letters' 0 commits "$(printf '%072d' 0 | sed 's/0/é/g')"
expect 'several subjects, one long: refused' 1 commits "Short subject
$(letters 80)
Another short one"
expect 'long merge of a branch: passes' 0 commits "Merge branch 'a-branch-name-long-enough-to-take-the-subject-past-the-limit' into batch-3"
expect 'long merge of a remote-tracking branch: passes' 0 commits "Merge remote-tracking branch 'origin/a-branch-name-long-enough-to-pass-the-limit-of-72'"
expect 'long merge from another repository: passes' 0 commits "Merge branch 'main' of https://github.com/someone/some-repository-with-a-long-name into batch-3"
expect 'long merge subject in a title: refused' 1 title "Merge branch 'a-branch-name-long-enough-to-take-the-subject-past-the-limit' into batch-3"
expect 'long subject that only starts like a merge: refused' 1 commits "Merge branch 'main' and fix the installer's handling of storages with little free space"
expect 'unknown kind: refused' 2 subject 'A subject'

out=$(printf '%s\n' "$(letters 73)" | sh "${script}" title 2>&1) || true
case ${out} in
    *'the title is 73 characters, 72 at most'*) passes=$((passes + 1)) ;;
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
