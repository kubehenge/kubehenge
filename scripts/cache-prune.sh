#!/bin/sh
# Keeps the repository's Actions cache within its 10 GB, with the GitHub CLI (GH_TOKEN with
# actions: write):
#
#   scripts/cache-prune.sh newest <ref>   # on <ref>, every family's newest entry alone
#   scripts/cache-prune.sh all <ref>      # every entry on <ref>, as for a closed pull request
#
# A family is one cache under the keys it moves through: ci-, website-, e2e-go- and e2e-pve-, each
# e2e template by its name (e2e-template-<name>-), and each fuzz target (fuzz-<package>-<target>-).
# A key of no family is left alone.
set -eu

mode=${1:?usage: $0 newest|all <ref>}
ref=${2:?usage: $0 newest|all <ref>}
repo=${GITHUB_REPOSITORY:?}

entries=$(gh api --paginate -X GET "repos/${repo}/actions/caches" -f ref="${ref}" -f per_page=100 \
    --jq '.actions_caches[] | "\(.id) \(.created_at) \(.size_in_bytes) \(.key)"')
case "${mode}" in
    all) doomed=$(printf '%s\n' "${entries}" | grep . || true) ;;
    newest)
        # Newest first, so the first entry of each family stays.
        doomed=$(printf '%s\n' "${entries}" | grep . | sort -k2,2r | awk '{
            key = $4; family = ""
            if (match(key, /^(ci|website|e2e-go|e2e-pve)-/)) family = substr(key, 1, RLENGTH)
            else if (key ~ /^e2e-template-.*-[0-9a-f]+$/) { family = key; sub(/[0-9a-f]+$/, "", family) }
            else if (key ~ /^fuzz-.*-[0-9]+$/) { family = key; sub(/[0-9]+$/, "", family) }
            if (family == "") next
            if (family in kept) print; else kept[family] = 1
        }' || true)
        ;;
    *)
        echo "usage: $0 newest|all <ref>" >&2
        exit 2
        ;;
esac
[ -n "${doomed}" ] || {
    echo "nothing to delete on ${ref}"
    exit 0
}
printf '%s\n' "${doomed}" | while read -r id created size key; do
    gh api -X DELETE "repos/${repo}/actions/caches/${id}" > /dev/null
    echo "deleted ${key} ($((size / 1048576)) MiB, saved ${created}) on ${ref}"
done
