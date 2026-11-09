#!/bin/sh
# Refuses a pull request title over 64 characters or a commit subject over 72, one per line on stdin,
# and prints each with its length. A title gets 64 because the squash commit on main adds " (#N)" to
# it. A merge subject in the form git writes it is let through, since git puts the branch names in it.
#
#   printf '%s\n' "${title}" | scripts/subjects.sh title
#   git log --format=%s main..HEAD | scripts/subjects.sh commits
set -eu

kind=${1:?usage: $0 title | commits}
case ${kind} in
    title) max=64 ;;
    commits) max=72 ;;
    *)
        printf 'subjects: unknown kind %s, title or commits\n' "${kind}" >&2
        exit 2
        ;;
esac

# Under LC_ALL=C every awk counts bytes, so UTF-8's continuation bytes are dropped before counting.
LC_ALL=C awk -v kind="${kind}" -v max="${max}" '
    {
        read = $0
        gsub(/[\200-\277]/, "", read)
    }
    length(read) <= max + 0 { next }
    kind == "commits" && /^Merge (remote-tracking )?branch '\''[^'\'']+'\''( of [^ ]+)?( into [^ ]+)?$/ { next }
    {
        printf "subjects: the %s is %d characters, %d at most: %s\n", kind == "title" ? "title" : "commit subject", length(read), max, $0 > "/dev/stderr"
        bad = 1
    }
    END { exit bad }
'
